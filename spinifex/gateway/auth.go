package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mulgadc/predastore/pkg/sigv4"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// AKID prefixes. Prefix-first dispatch prevents a misfiled record from being
// resolved by the wrong lookup path.
const (
	longLivedAKIDPrefix = "AKIA"
	sessionAKIDPrefix   = "ASIA"
)

// SigV4AuthMiddleware returns stdlib middleware that validates AWS Signature V4 authentication.
func (gw *GatewayConfig) SigV4AuthMiddleware() func(http.Handler) http.Handler {
	if gw.RateLimiter == nil {
		gw.RateLimiter = NewAuthRateLimiter()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			clientIP := utils.ClientIP(r.RemoteAddr)
			if errCode := gw.RateLimiter.CheckIP(clientIP); errCode != "" {
				gw.writeSigV4Error(w, r, errCode)
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
					gw.writeSigV4Error(w, r, awserrors.ErrorMissingAuthenticationToken)
				case errors.Is(err, sigv4.ErrPayloadTooLarge):
					gw.writeSigV4Error(w, r, awserrors.ErrorRequestEntityTooLarge)
				case errors.Is(err, sigv4.ErrRequestTimeTooSkewed):
					// Skew/replay: AWS returns this as SignatureDoesNotMatch, which is
					// indistinguishable on the wire from a canonicalisation mismatch.
					// Log the timestamps so the two can be told apart after the fact.
					slog.Warn("Auth failure: request time too skewed",
						"sourceIP", clientIP,
						"requestTime", signingTime(r),
						"serverTime", time.Now().UTC().Format("20060102T150405Z"),
						"maxSkew", sigv4.MaxClockSkew)
					gw.RateLimiter.RecordFailure(clientIP)
					gw.writeSigV4Error(w, r, awserrors.ErrorSignatureDoesNotMatch)
				default:
					// Malformed Authorization, bad credential scope, unsupported
					// algorithm, missing content hash: a failed auth attempt. The parse
					// error names which, and is the only record of it.
					slog.Warn("Auth failure: malformed signature envelope",
						"sourceIP", clientIP, "err", err)
					gw.RateLimiter.RecordFailure(clientIP)
					gw.writeSigV4Error(w, r, awserrors.ErrorIncompleteSignature)
				}
				return
			}

			// Reject unknown services before crypto; otherwise Verify re-signs with
			// the client-claimed service name and rubber-stamps the scope.
			if !supportedServices[sig.Credential.Service] {
				// Also surfaces as SignatureDoesNotMatch, so name the service that was
				// rejected rather than leaving it to look like a signing bug.
				slog.Warn("Auth failure: unsupported service in credential scope",
					"accessKeyID", sig.Credential.AccessKeyID, "sourceIP", clientIP,
					"service", sig.Credential.Service)
				gw.writeSigV4Error(w, r, awserrors.ErrorSignatureDoesNotMatch)
				return
			}

			// Fail fast when NATS is down; LookupAccessKey would hang 5 s otherwise.
			// Nil NATSConn (test helpers) skips this check.
			if gw.NATSConn != nil && !gw.NATSConn.IsConnected() {
				gw.writeClusterUnavailable(w, r, sig.Credential.Service)
				return
			}

			if gw.IAMService == nil {
				slog.Error("SigV4 auth: IAM service not initialized")
				gw.writeSigV4Error(w, r, awserrors.ErrorInternalError)
				return
			}

			var (
				secret     string
				principal  principalContext
				lookupCode string
			)
			switch {
			case strings.HasPrefix(sig.Credential.AccessKeyID, longLivedAKIDPrefix):
				secret, principal, lookupCode = gw.resolveLongLivedAKID(sig.Credential.AccessKeyID, clientIP)
			case strings.HasPrefix(sig.Credential.AccessKeyID, sessionAKIDPrefix):
				secret, principal, lookupCode = gw.resolveSessionAKID(r, sig.Credential.AccessKeyID, clientIP)
			default:
				slog.Warn("Auth failure: unknown AKID prefix", "accessKeyID", sig.Credential.AccessKeyID, "sourceIP", clientIP)
				gw.RateLimiter.RecordFailure(clientIP)
				gw.writeSigV4Error(w, r, awserrors.ErrorInvalidClientTokenId)
				return
			}
			if lookupCode != "" {
				gw.writeSigV4Error(w, r, lookupCode)
				return
			}

			// Region is pinned to the gateway; service is the client-claimed scope,
			// already gated against supportedServices above.
			if _, err := sig.Verify(secret, gw.Region, sig.Credential.Service); err != nil {
				// A mismatch means our canonical request differs from the one the client
				// signed, so log ours to diff against the SDK's. Warn, not Debug: these
				// failures are intermittent and never reproduce on demand.
				slog.Warn("Auth failure: verification failed",
					"accessKeyID", sig.Credential.AccessKeyID, "sourceIP", clientIP,
					"service", sig.Credential.Service,
					"action", mismatchAction(r, sig.Credential.Service),
					"canonicalRequest", redactedCanonicalRequest(sig),
					"err", err)
				gw.RateLimiter.RecordFailure(clientIP)
				gw.writeSigV4Error(w, r, awserrors.ErrorSignatureDoesNotMatch)
				return
			}

			// Parse rewound the body; re-read it for query-arg parsing, then rewind
			// again for the downstream handler.
			body, err := io.ReadAll(r.Body)
			if err != nil {
				slog.Error("Failed to read request body", "err", err)
				gw.writeSigV4Error(w, r, awserrors.ErrorInternalError)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))

			ctx := r.Context()
			ctx = context.WithValue(ctx, ctxIdentity, principal.identity)
			ctx = context.WithValue(ctx, ctxAccountID, principal.accountID)
			ctx = context.WithValue(ctx, ctxService, sig.Credential.Service)
			ctx = context.WithValue(ctx, ctxRegion, sig.Credential.Region)
			ctx = context.WithValue(ctx, ctxAccessKey, sig.Credential.AccessKeyID)
			ctx = context.WithValue(ctx, ctxPrincipalType, principal.principalType)
			if principal.assumedRoleARN != "" {
				ctx = context.WithValue(ctx, ctxAssumedRoleARN, principal.assumedRoleARN)
			}
			if principal.assumedRoleID != "" {
				ctx = context.WithValue(ctx, ctxAssumedRoleID, principal.assumedRoleID)
			}
			if principal.underlyingRoleARN != "" {
				ctx = context.WithValue(ctx, ctxUnderlyingRoleARN, principal.underlyingRoleARN)
			}

			// Parse once; dispatchers reuse via ctxQueryArgs. On error the
			// dispatcher re-parses and returns MalformedQueryString.
			if args, err := ParseAWSQueryArgs(string(body)); err == nil {
				ctx = context.WithValue(ctx, ctxQueryArgs, args)
				if action := args["Action"]; action != "" {
					ctx = context.WithValue(ctx, ctxAction, action)
				}
			}

			slog.Debug("SigV4 authentication successful",
				"accessKey", sig.Credential.AccessKeyID,
				"identity", principal.identity,
				"principalType", principal.principalType)
			gw.RateLimiter.RecordSuccess(clientIP)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// redactedCanonicalRequest renders the server's canonical request for a mismatch log with
// the session token masked. The rest is safe: it holds signed header values and a payload
// hash, never the secret key.
func redactedCanonicalRequest(sig *sigv4.SignedRequest) string {
	lines := strings.Split(sig.CanonicalRequest(), "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "x-amz-security-token:") {
			lines[i] = "x-amz-security-token:<redacted>"
		}
	}

	return strings.Join(lines, "\n")
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

// principalContext is the identity envelope set on the request context after
// SigV4 verification succeeds.
type principalContext struct {
	identity          string
	accountID         string
	principalType     string
	assumedRoleARN    string
	assumedRoleID     string
	underlyingRoleARN string
}

// resolveLongLivedAKID handles the AKIA path: IAM lookup, status check, secret decrypt.
func (gw *GatewayConfig) resolveLongLivedAKID(accessKeyID, clientIP string) (string, principalContext, string) {
	ak, err := gw.IAMService.LookupAccessKey(accessKeyID)
	if err != nil {
		if strings.Contains(err.Error(), awserrors.ErrorIAMNoSuchEntity) {
			slog.Warn("Auth failure: access key not found", "accessKeyID", accessKeyID, "sourceIP", clientIP)
			gw.RateLimiter.RecordFailure(clientIP)
			return "", principalContext{}, awserrors.ErrorInvalidClientTokenId
		}
		slog.Error("IAM lookup failed", "accessKeyID", accessKeyID, "err", err)
		return "", principalContext{}, awserrors.ErrorInternalError
	}
	if ak.Status != handlers_iam.AccessKeyStatusActive {
		slog.Warn("Auth failure: access key inactive", "accessKeyID", accessKeyID, "sourceIP", clientIP)
		gw.RateLimiter.RecordFailure(clientIP)
		return "", principalContext{}, awserrors.ErrorInvalidClientTokenId
	}
	secret, err := gw.IAMService.DecryptSecret(ak.SecretAccessKey)
	if err != nil {
		// Undecryptable secret (e.g. master key rotated): treat as auth failure, not
		// server fault, so the client re-authenticates instead of retrying a dead request.
		slog.Error("Failed to decrypt IAM secret", "accessKeyID", accessKeyID, "err", err)
		gw.RateLimiter.RecordFailure(clientIP)
		return "", principalContext{}, awserrors.ErrorInvalidClientTokenId
	}
	return secret, principalContext{
		identity:      ak.UserName,
		accountID:     ak.AccountID,
		principalType: principalTypeUser,
	}, ""
}

// resolveSessionAKID handles the ASIA path: STS lookup, expiry check,
// X-Amz-Security-Token HMAC verify, secret decrypt. Nil STSService surfaces
// as InternalError so misconfiguration is loud at startup.
func (gw *GatewayConfig) resolveSessionAKID(r *http.Request, accessKeyID, clientIP string) (string, principalContext, string) {
	if gw.STSService == nil {
		slog.Error("SigV4 auth: STS service not initialized but session AKID presented", "accessKeyID", accessKeyID)
		return "", principalContext{}, awserrors.ErrorInternalError
	}
	cred, err := gw.STSService.LookupSessionCredential(accessKeyID)
	if err != nil {
		slog.Error("STS lookup failed", "accessKeyID", accessKeyID, "err", err)
		return "", principalContext{}, awserrors.ErrorInternalError
	}
	if cred == nil {
		slog.Warn("Auth failure: session credential not found", "accessKeyID", accessKeyID, "sourceIP", clientIP)
		gw.RateLimiter.RecordFailure(clientIP)
		return "", principalContext{}, awserrors.ErrorInvalidClientTokenId
	}
	if time.Now().UTC().After(cred.ExpiresAt) {
		slog.Warn("Auth failure: session credential expired",
			"accessKeyID", accessKeyID, "sourceIP", clientIP, "expiresAt", cred.ExpiresAt)
		gw.RateLimiter.RecordFailure(clientIP)
		return "", principalContext{}, awserrors.ErrorExpiredToken
	}

	tokenHeader := r.Header.Get("X-Amz-Security-Token")
	if tokenHeader == "" {
		slog.Warn("Auth failure: session AKID presented without X-Amz-Security-Token",
			"accessKeyID", accessKeyID, "sourceIP", clientIP)
		gw.RateLimiter.RecordFailure(clientIP)
		return "", principalContext{}, awserrors.ErrorInvalidClientTokenId
	}
	if !gw.STSService.VerifySessionToken(cred, tokenHeader) {
		slog.Warn("Auth failure: session token HMAC mismatch",
			"accessKeyID", accessKeyID, "sourceIP", clientIP)
		gw.RateLimiter.RecordFailure(clientIP)
		return "", principalContext{}, awserrors.ErrorInvalidClientTokenId
	}

	secret, err := gw.IAMService.DecryptSecret(cred.SecretEncrypted)
	if err != nil {
		// Unverifiable secret: same auth-failure reasoning as resolveLongLivedAKID.
		slog.Error("Failed to decrypt session secret", "accessKeyID", accessKeyID, "err", err)
		gw.RateLimiter.RecordFailure(clientIP)
		return "", principalContext{}, awserrors.ErrorInvalidClientTokenId
	}
	if cred.PrincipalType == principalTypeUser {
		// GetSessionToken session: resolve to the user so policy is evaluated as a user.
		return secret, principalContext{
			identity:      cred.SessionName,
			accountID:     cred.AccountID,
			principalType: principalTypeUser,
		}, ""
	}

	// "assumed-role" or empty (pre-PrincipalType records) — both resolve as role session.
	return secret, principalContext{
		identity:          cred.SessionName,
		accountID:         cred.AccountID,
		principalType:     principalTypeAssumedRole,
		assumedRoleARN:    cred.AssumedRoleARN,
		assumedRoleID:     cred.AssumedRoleID,
		underlyingRoleARN: cred.UnderlyingRoleARN,
	}, ""
}

// writeSigV4Error writes an EC2-compatible XML error response for auth failures.
func (gw *GatewayConfig) writeSigV4Error(w http.ResponseWriter, r *http.Request, errorCode string) {
	requestID := uuid.NewString()

	errorMsg, exists := awserrors.ErrorLookup[errorCode]
	if !exists {
		errorMsg = awserrors.ErrorMessage{HTTPCode: 500, Message: "Internal error"}
	}

	xmlError := GenerateEC2ErrorResponse(errorCode, errorMsg.Message, requestID)

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(errorMsg.HTTPCode)
	_, _ = w.Write(xmlError)
}
