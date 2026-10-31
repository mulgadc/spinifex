package gateway

import (
	"bytes"
	"errors"
	authlimit "github.com/mulgadc/spinifex/spinifex/ingress/aws/ratelimit"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/reqctx"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/accountadmission"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"uuid"

	"github.com/mulgadc/bluebottle/pkg/sigv4"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	telemetry "github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/audit"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/credential"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/envelope"
	ingresshttp "github.com/mulgadc/spinifex/spinifex/ingress/http"
)

// authSource is the caller's address as the auth path uses it. The rate limiter
// keys on the connection peer; log lines use the loopback-gated client IP so they
// agree with the request audit line for proxied requests.
type authSource struct {
	limitKey string
	logIP    string
}

// SigV4AuthMiddleware returns stdlib middleware that validates AWS Signature V4 authentication.
func (gw *GatewayConfig) SigV4AuthMiddleware() func(http.Handler) http.Handler {
	if gw.RateLimiter == nil {
		gw.RateLimiter = authlimit.NewAuthRateLimiter()
	}
	if gw.Admission == nil {
		gw.Admission = accountadmission.New(gw.IAMService)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			src := authSource{limitKey: ingresshttp.ClientIP(r.RemoteAddr), logIP: ingresshttp.RequestClientIP(r)}
			if errCode := gw.RateLimiter.CheckIP(src.limitKey); errCode != "" {
				gw.writeSigV4Error(w, r, errCode, "")
				return
			}

			// sigv4.Parse reads, hashes, and rewinds the request body to build the
			// canonical request; Verify below only recomputes the HMAC.
			sig, err := sigv4.Parse(r)
			if err != nil {
				// sigv4 validates the envelope, credential scope, and timestamp at
				// parse time. Distinguish a request that never presented credentials
				// (or is simply too large) from a failed authentication attempt: the
				// latter is rate-limited so a brute-forcer is locked out regardless of
				// which validation stage rejects it.
				switch {
				case errors.Is(err, sigv4.ErrMissingAuthentication):
					gw.writeSigV4Error(w, r, awserrors.ErrorMissingAuthenticationToken, "")
				case errors.Is(err, sigv4.ErrPayloadTooLarge):
					gw.writeSigV4Error(w, r, awserrors.ErrorRequestEntityTooLarge, "")
				case errors.Is(err, sigv4.ErrReadingBody):
					// A dropped or stalled connection, not an auth attempt: counting it
					// would lock out an IP for its network trouble.
					slog.Warn("Auth aborted: request body read failed",
						"sourceIP", src.logIP, "err", err)
					gw.writeSigV4Error(w, r, awserrors.ErrorInternalError, "")
				case errors.Is(err, sigv4.ErrRequestTimeTooSkewed):
					// Skew/replay: AWS returns this as SignatureDoesNotMatch, which is
					// indistinguishable on the wire from a canonicalisation mismatch.
					// Log the timestamps so the two can be told apart after the fact.
					slog.Warn("Auth failure: request time too skewed",
						"sourceIP", src.logIP,
						"requestTime", signingTime(r),
						"serverTime", time.Now().UTC().Format("20060102T150405Z"),
						"max_skew_ms", telemetry.Millis(sigv4.MaxClockSkew))
					// Anonymous: the request was rejected before its key id was parsed,
					// so there is no client identity for the lockout to protect.
					gw.RateLimiter.RecordFailure(src.limitKey, authlimit.Anonymous)
					gw.writeSigV4Error(w, r, awserrors.ErrorSignatureDoesNotMatch, "")
				default:
					// Malformed Authorization, bad credential scope, unsupported
					// algorithm, missing content hash: a failed auth attempt. The parse
					// error names which, and is the only record of it.
					slog.Warn("Auth failure: malformed signature envelope",
						"sourceIP", src.logIP, "err", err)
					gw.RateLimiter.RecordFailure(src.limitKey, authlimit.Anonymous)
					gw.writeSigV4Error(w, r, awserrors.ErrorIncompleteSignature, "")
				}
				return
			}

			// The key a request presented, recorded before it is known to be valid:
			// a rejected caller has to be identifiable, and this is the last point
			// every rejection below still shares.
			audit.From(r.Context()).SetAccessKeyID(sig.Credential.AccessKeyID)

			// Carry the signed service now, so a rejection below (a signature
			// mismatch on a bedrock Converse) renders in the client's format —
			// GetService reads this to pick JSON over XML for the bedrock family.
			r = r.WithContext(reqctx.WithService(r.Context(), sig.Credential.Service))

			// Reject unknown services before crypto; otherwise Verify re-signs with
			// the client-claimed service name and rubber-stamps the scope. A
			// credential-scope service is served if the registration seam carries it.
			if !gw.selector().Served(sig.Credential.Service) {
				slog.Warn("Auth failure: unsupported service in credential scope",
					"accessKeyID", sig.Credential.AccessKeyID, "sourceIP", src.logIP,
					"service", sig.Credential.Service)
				// Consistent with an unimplemented action on a served service: this
				// gate runs before Verify, so nothing was ever wrong with the
				// credentials, and no signature was ever checked.
				code, message := dispatch.UnknownService(sig.Credential.Service)
				gw.writeSigV4Error(w, r, code, message)
				return
			}

			// Fail fast when NATS is down; LookupAccessKey would hang 5 s otherwise.
			// Nil NATSConn (test helpers) skips this check.
			if gw.NATSConn != nil && !gw.NATSConn.IsConnected() {
				gw.writeClusterUnavailable(w, r, sig.Credential.Service)
				return
			}

			if gw.Credentials == nil {
				// No verifier means no way to authenticate: fail closed, never allow.
				slog.Error("SigV4 auth: credential verifier not configured")
				gw.writeSigV4Error(w, r, awserrors.ErrorInternalError, "")
				return
			}

			credReq := credential.Request{
				AccessKeyID:   sig.Credential.AccessKeyID,
				SecurityToken: r.Header.Get("X-Amz-Security-Token"),
				SourceIP:      src.logIP,
			}

			// Region is the gateway's, or a global service's fixed signing region;
			// service is the client-claimed scope, gated against the registered services above.
			var sigErr error
			authed, f := gw.Credentials.Authenticate(credReq, func(secret string) error {
				_, sigErr = sig.Verify(secret, gw.expectedSigningRegion(sig.Credential.Service, sig.Credential.Region), sig.Credential.Service)
				return sigErr
			})
			if f != nil {
				if f.SignatureMismatch {
					// A mismatch means our canonical request differs from the one the client
					// signed, so log ours to diff against the SDK's. Warn, not Debug: these
					// failures are intermittent and never reproduce on demand.
					slog.Warn("Auth failure: verification failed",
						"accessKeyID", sig.Credential.AccessKeyID, "sourceIP", src.logIP,
						"service", sig.Credential.Service,
						"action", mismatchAction(r, sig.Credential.Service),
						"canonicalRequest", sig.RedactedCanonicalRequest(),
						"err", sigErr)
					// Fingerprinted by the signature as well as the key id: each guess at a
					// secret produces a different one, while a client retrying an identical
					// bad request produces the same one.
					gw.RateLimiter.RecordFailure(src.limitKey, authlimit.Fingerprint("signature", sig.Credential.AccessKeyID, sig.Signature))
				} else {
					gw.recordCredentialFailure(src, f)
				}
				gw.writeSigV4Error(w, r, f.Code, "")
				return
			}

			// Only after the signature holds: an unauthenticated prober must not
			// be able to use this to tell a suspended account from a live one.
			if errCode := gw.checkAccountActive(authed.AccountID, src); errCode != "" {
				gw.writeSigV4Error(w, r, errCode, "")
				return
			}

			// Principal state and the caller's user ID, resolved here once rather than
			// per policy check: a fault must fail the request rather than authorize it
			// against a context missing the key.
			caller, f := gw.Credentials.Resolve(credReq, authed)
			if f != nil {
				gw.recordCredentialFailure(src, f)
				gw.writeSigV4Error(w, r, f.Code, "")
				return
			}
			// Parse rewound the body; re-read it for query-arg parsing, then rewind
			// again for the downstream handler.
			body, err := io.ReadAll(r.Body)
			if err != nil {
				slog.Error("Failed to read request body", "err", err)
				gw.writeSigV4Error(w, r, awserrors.ErrorInternalError, "")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))

			ctx := r.Context()
			ctx = reqctx.WithPrincipal(ctx, caller, sig.Credential.AccessKeyID)
			ctx = reqctx.WithService(ctx, sig.Credential.Service)
			ctx = reqctx.WithRegion(ctx, sig.Credential.Region)
			audit.From(ctx).SetIdentity(sig.Credential.AccessKeyID, caller.AccountID,
				sig.Credential.Region, sig.Credential.Service, caller.PrincipalType)

			// A malformed body keeps the action unset; the dispatcher re-parses it and
			// returns MalformedQueryString.
			if args, err := ParseAWSQueryArgs(string(body)); err == nil {
				if action := args["Action"]; action != "" {
					ctx = reqctx.WithAction(ctx, action)
				}
			}

			// A registered service names its own action; an
			// empty answer keeps the query-protocol Action parsed above.
			if action := gw.selector().ResolveAction(r, sig.Credential.Service); action != "" {
				ctx = reqctx.WithAction(ctx, action)
			}

			slog.Debug("SigV4 authentication successful",
				"accessKey", sig.Credential.AccessKeyID,
				"identity", caller.Identity,
				"principalType", caller.PrincipalType)
			gw.RateLimiter.RecordSuccess(src.limitKey)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// resolveNonQueryAction resolves the action for JSON-1.1 (X-Amz-Target) and
// path-routed REST-JSON services (eks), which carry no
// query-protocol Action parameter. Empty on no match.
func resolveNonQueryAction(r *http.Request) string {
	if target := r.Header.Get("X-Amz-Target"); target != "" {
		if _, after, ok := strings.CutLast(target, "."); ok {
			return after
		}
		return target
	}

	return ""
}

// signingTime returns the timestamp the client signed with, read straight off the
// request. Parse rejects a skewed request before returning a SignedRequest, so the
// raw header (or the presigned query arg) is the only copy available to log.
func signingTime(r *http.Request) string {
	if ts := r.Header.Get("X-Amz-Date"); ts != "" {
		return ts
	}
	if ts := r.URL.Query().Get("X-Amz-Date"); ts != "" {
		return ts
	}
	return r.Header.Get("Date")
}

// mismatchAction recovers the Action for a rejected query-protocol request so the log names
// the operation. Auth fails before the dispatcher parses it, which otherwise leaves the
// trace as a bare "POST /". S3 is skipped: its body is object data, not query args.
func mismatchAction(r *http.Request, service string) string {
	if service == "s3" {
		return ""
	}

	if action := r.URL.Query().Get("Action"); action != "" {
		return action
	}

	// Parse rewound the body, so this re-reads the same bytes it hashed.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return ""
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	args, err := ParseAWSQueryArgs(string(body))
	if err != nil {
		return ""
	}

	return args["Action"]
}

// recordCredentialFailure counts a verifier rejection toward the source's lockout.
func (gw *GatewayConfig) recordCredentialFailure(src authSource, f *credential.Failure) {
	if f.FingerprintKind == "" {
		return
	}
	gw.RateLimiter.RecordFailure(src.limitKey, authlimit.Fingerprint(f.FingerprintKind, f.FingerprintParts...))
}

// writeSigV4Error writes an auth-failure error in the service-appropriate
// format. Every auth rejection funnels through here, a rate-limit lockout
// included, so this is the one place that has to record the verdict onto the
// request. message overrides the registry text from ErrorLookup when the
// call site knows a more truthful reason to give; pass "" to use the
// registry text unchanged.
func (gw *GatewayConfig) writeSigV4Error(w http.ResponseWriter, r *http.Request, errorCode, message string) {
	requestID := uuid.NewV4().String()
	audit.From(r.Context()).SetAuthError(errorCode)

	errorMsg, exists := awserrors.ErrorLookup[errorCode]
	if !exists {
		errorMsg = awserrors.ErrorMessage{HTTPCode: 500, Message: "Internal error"}
	}
	if message != "" {
		errorMsg.Message = message
	}

	svc := signedService(r)
	if svc == "" {
		// A request rejected while its signature was still being parsed has no
		// service in context; its claimed scope still picks a registration's
		// envelope, and nothing else.
		if hint := credentialScopeService(r); hint != "" {
			if _, ok := gw.registered(hint); ok {
				svc = hint
			}
		}
	}

	// JSON services (bedrock family, EKS, registered JSON services, …) need a
	// JSON error body in their own protocol, or the SDK chokes deserializing
	// our XML into its shape. An unserved scope is absent from the tables by
	// definition, so its protocol is read off the request itself instead.
	p, isJSON := gw.errorProtocol(svc)
	if !isJSON && envelope.RequestSignalsJSONProtocol(r) {
		p, isJSON = envelope.ProtocolJSON11, true
	}
	if isJSON {
		if p == envelope.ProtocolJSON10Query && errorCode == awserrors.ErrorRequestEntityTooLarge {
			envelope.WriteTooLargeText(w, int64(sigv4.MaxPayloadLen))
			return
		}
		gw.writeJSONError(w, p, svc, envelope.JSONError{Code: errorCode, Message: errorMsg.Message, Status: errorMsg.HTTPCode})
		return
	}

	xmlError := xmlErrorBody(svc, errorCode, errorMsg.Message, requestID, r.URL.Path)

	w.Header().Set("Content-Type", envelope.XMLContentType)
	w.WriteHeader(errorMsg.HTTPCode)
	_, _ = w.Write(xmlError)
}

// credentialScopeService returns the service named in the request's
// Authorization credential scope, unverified, or "" when it has none.
func credentialScopeService(r *http.Request) string {
	_, rest, ok := strings.Cut(r.Header.Get("Authorization"), "Credential=")
	if !ok {
		return ""
	}
	scope, _, _ := strings.Cut(rest, ",")
	parts := strings.Split(strings.TrimSpace(scope), "/")
	if len(parts) != 5 {
		return ""
	}
	return parts[3]
}

// signedService returns the credential-scope service the client signed with,
// including a scope the gateway does not serve. A rejection has to render in
// the client's format, and GetService reports those scopes as empty.
func signedService(r *http.Request) string {
	return reqctx.Service(r.Context())
}

// globalSigningRegions maps a global AWS service to the region its clients sign
// with when they use the default endpoint. A client given an endpoint override
// signs with its own configured region instead, so both are accepted.
var globalSigningRegions = map[string]string{
	"route53": "us-east-1",
}

// expectedSigningRegion returns the credential-scope region a request for
// service must carry: the global service's signing region when the client
// claimed it, else the gateway's own region.
func (gw *GatewayConfig) expectedSigningRegion(service, claimed string) string {
	if global, ok := globalSigningRegions[service]; ok && claimed == global {
		return global
	}
	return gw.Region
}
