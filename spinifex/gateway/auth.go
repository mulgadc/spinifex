package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mulgadc/predastore/auth"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// Maximum request body size for signature validation (10 MB).
const maxBodySize = 10 * 1024 * 1024

// AKID prefixes. Branching on the prefix BEFORE any IAM/STS lookup eliminates
// a class of bypass where a misfiled record could be resolved by the wrong
// path.
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

			sig, err := auth.ParseReq(r)
			if err != nil {
				gw.writeSigV4Error(w, r, parseSigV4ErrorCode(err))
				return
			}

			// Reject services the gateway doesn't serve before any crypto:
			// otherwise Verify re-signs with the client-claimed service and
			// rubber-stamps the scope.
			if !supportedServices[sig.Service] {
				gw.writeSigV4Error(w, r, awserrors.ErrorSignatureDoesNotMatch)
				return
			}

			// Fail fast when NATS is down: LookupAccessKey waits 5s per attempt
			// before context.DeadlineExceeded, and gateway.Request()'s catch-all
			// IsConnected check is unreachable from here. Nil NATSConn (test
			// helpers) skips the short-circuit.
			if gw.NATSConn != nil && !gw.NATSConn.IsConnected() {
				gw.writeClusterUnavailable(w, r, sig.Service)
				return
			}

			if gw.IAMService == nil {
				slog.Error("SigV4 auth: IAM service not initialized")
				gw.writeSigV4Error(w, r, awserrors.ErrorInternalError)
				return
			}

			// Resolve the AKID to a secret + principal context. Prefix-first
			// dispatch — AWS does not mint AKIDs outside these two namespaces,
			// and the bucket-prefix invariant guarantees an AKIA AKID cannot
			// land in the session bucket or vice versa.
			var (
				secret     string
				principal  principalContext
				lookupCode string
			)
			switch {
			case strings.HasPrefix(sig.AccessKeyID, longLivedAKIDPrefix):
				secret, principal, lookupCode = gw.resolveLongLivedAKID(sig.AccessKeyID, clientIP)
			case strings.HasPrefix(sig.AccessKeyID, sessionAKIDPrefix):
				secret, principal, lookupCode = gw.resolveSessionAKID(r, sig.AccessKeyID, clientIP)
			default:
				slog.Warn("Auth failure: unknown AKID prefix", "accessKeyID", sig.AccessKeyID, "sourceIP", clientIP)
				gw.RateLimiter.RecordFailure(clientIP)
				gw.writeSigV4Error(w, r, awserrors.ErrorInvalidClientTokenId)
				return
			}
			if lookupCode != "" {
				gw.writeSigV4Error(w, r, lookupCode)
				return
			}

			r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
			body, err := io.ReadAll(r.Body)
			if err != nil {
				var maxBytesErr *http.MaxBytesError
				if errors.As(err, &maxBytesErr) {
					slog.Warn("Request body too large", "limit", maxBodySize)
					gw.writeSigV4Error(w, r, awserrors.ErrorRequestEntityTooLarge)
					return
				}
				slog.Error("Failed to read request body", "err", err)
				gw.writeSigV4Error(w, r, awserrors.ErrorInternalError)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))

			sum := sha256.Sum256(body)
			bodyHash := hex.EncodeToString(sum[:])

			if err := sig.Verify(secret, sig.Service, gw.Region,
				auth.WithBodyHash(bodyHash)); err != nil {
				attrs := []any{
					"accessKeyID", sig.AccessKeyID,
					"sourceIP", clientIP,
					"err", err,
				}
				var sme *auth.SigMismatchError
				if errors.As(err, &sme) {
					attrs = append(attrs, "expectedAuthHdr", sme.ExpectedAuthHdr, "providedAuthHdr", sme.ProvidedAuthHdr)
				}
				slog.Warn("Auth failure: verification failed", attrs...)
				gw.RateLimiter.RecordFailure(clientIP)
				gw.writeSigV4Error(w, r, verifySigV4ErrorCode(err))
				return
			}

			ctx := r.Context()
			ctx = context.WithValue(ctx, ctxIdentity, principal.identity)
			ctx = context.WithValue(ctx, ctxAccountID, principal.accountID)
			ctx = context.WithValue(ctx, ctxService, sig.Service)
			ctx = context.WithValue(ctx, ctxRegion, sig.Region)
			ctx = context.WithValue(ctx, ctxAccessKey, sig.AccessKeyID)
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

			// Parse once; dispatchers reuse via ctxQueryArgs. On error, the
			// dispatcher re-parses and returns MalformedQueryString.
			if args, err := ParseAWSQueryArgs(string(body)); err == nil {
				ctx = context.WithValue(ctx, ctxQueryArgs, args)
				if action := args["Action"]; action != "" {
					ctx = context.WithValue(ctx, ctxAction, action)
				}
			}

			slog.Debug("SigV4 authentication successful",
				"accessKey", sig.AccessKeyID,
				"identity", principal.identity,
				"principalType", principal.principalType)
			gw.RateLimiter.RecordSuccess(clientIP)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
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

// resolveLongLivedAKID handles the AKIA path: IAMService lookup, status check,
// secret decryption. Returns an error code from awserrors on failure.
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
		// An undecryptable secret means the stored credential cannot be used to
		// verify the request — e.g. the IAM master key was rotated out from
		// under a key record left behind in KV. The credential is unverifiable,
		// so this is an authentication failure, not a server fault: AWS never
		// returns InternalError for a bad credential, and InvalidClientTokenId
		// lets the client re-authenticate instead of retrying a dead request.
		// The ERROR log preserves the signal for an operator diagnosing a
		// genuine cipher misconfiguration.
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
// X-Amz-Security-Token HMAC verification, secret decryption. A missing STS
// service is a configuration error and surfaces as InternalError so the
// problem is loud at startup, not a silent allow.
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
		// Unverifiable session secret — treat as an authentication failure for
		// the same reason as the long-lived path above.
		slog.Error("Failed to decrypt session secret", "accessKeyID", accessKeyID, "err", err)
		gw.RateLimiter.RecordFailure(clientIP)
		return "", principalContext{}, awserrors.ErrorInvalidClientTokenId
	}
	if cred.PrincipalType == principalTypeUser {
		// Minted by GetSessionToken for an IAM user: resolve straight back to
		// that user so buildCallerARN yields arn:aws:iam::A:user/N and IAM
		// policy is evaluated against the user, not a synthesised role session.
		// The assumed-role fields stay empty by construction.
		return secret, principalContext{
			identity:      cred.SessionName,
			accountID:     cred.AccountID,
			principalType: principalTypeUser,
		}, ""
	}

	// "assumed-role" or empty — the latter covers records minted before
	// PrincipalType existed, so an absent value keeps the original role-session
	// behaviour (see SessionCredential.PrincipalType backward-compat note).
	return secret, principalContext{
		identity:          cred.SessionName,
		accountID:         cred.AccountID,
		principalType:     principalTypeAssumedRole,
		assumedRoleARN:    cred.AssumedRoleARN,
		assumedRoleID:     cred.AssumedRoleID,
		underlyingRoleARN: cred.UnderlyingRoleARN,
	}, ""
}

func parseSigV4ErrorCode(err error) string {
	switch {
	case errors.Is(err, auth.ErrMissingAuth):
		return awserrors.ErrorMissingAuthenticationToken
	default:
		return awserrors.ErrorIncompleteSignature
	}
}

func verifySigV4ErrorCode(err error) string {
	switch {
	case errors.Is(err, auth.ErrMissingContentSHA):
		return awserrors.ErrorIncompleteSignature
	default:
		return awserrors.ErrorSignatureDoesNotMatch
	}
}

// writeSigV4Error writes an EC2-compatible XML error response for authentication failures.
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
