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

	"github.com/google/uuid"
	"github.com/mulgadc/predastore/auth"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
)

// Maximum request body size for signature validation (10 MB).
const maxBodySize = 10 * 1024 * 1024

// SigV4AuthMiddleware returns stdlib middleware that validates AWS Signature V4 authentication.
func (gw *GatewayConfig) SigV4AuthMiddleware() func(http.Handler) http.Handler {
	if gw.RateLimiter == nil {
		gw.RateLimiter = NewAuthRateLimiter()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			clientIP := extractClientIP(r)
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
			ak, err := gw.IAMService.LookupAccessKey(sig.AccessKeyID)
			if err != nil {
				if strings.Contains(err.Error(), awserrors.ErrorIAMNoSuchEntity) {
					slog.Warn("Auth failure: access key not found", "accessKeyID", sig.AccessKeyID, "sourceIP", clientIP)
					gw.RateLimiter.RecordFailure(clientIP)
					gw.writeSigV4Error(w, r, awserrors.ErrorInvalidClientTokenId)
					return
				}
				slog.Error("IAM lookup failed", "accessKeyID", sig.AccessKeyID, "err", err)
				gw.writeSigV4Error(w, r, awserrors.ErrorInternalError)
				return
			}
			if ak.Status != handlers_iam.AccessKeyStatusActive {
				slog.Warn("Auth failure: access key inactive", "accessKeyID", sig.AccessKeyID, "sourceIP", clientIP)
				gw.RateLimiter.RecordFailure(clientIP)
				gw.writeSigV4Error(w, r, awserrors.ErrorInvalidClientTokenId)
				return
			}
			secret, err := gw.IAMService.DecryptSecret(ak.SecretAccessKey)
			if err != nil {
				slog.Error("Failed to decrypt IAM secret", "accessKeyID", sig.AccessKeyID, "err", err)
				gw.writeSigV4Error(w, r, awserrors.ErrorInternalError)
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
				slog.Warn("Auth failure: verification failed",
					"accessKeyID", sig.AccessKeyID,
					"sourceIP", clientIP,
					"err", err)
				gw.RateLimiter.RecordFailure(clientIP)
				gw.writeSigV4Error(w, r, verifySigV4ErrorCode(err))
				return
			}

			ctx := r.Context()
			ctx = context.WithValue(ctx, ctxIdentity, ak.UserName)
			ctx = context.WithValue(ctx, ctxAccountID, ak.AccountID)
			ctx = context.WithValue(ctx, ctxService, sig.Service)
			ctx = context.WithValue(ctx, ctxRegion, sig.Region)
			ctx = context.WithValue(ctx, ctxAccessKey, sig.AccessKeyID)

			// Parse once; dispatchers reuse via ctxQueryArgs. On error, the
			// dispatcher re-parses and returns MalformedQueryString.
			if args, err := ParseAWSQueryArgs(string(body)); err == nil {
				ctx = context.WithValue(ctx, ctxQueryArgs, args)
				if action := args["Action"]; action != "" {
					ctx = context.WithValue(ctx, ctxAction, action)
				}
			}

			slog.Debug("SigV4 authentication successful", "accessKey", sig.AccessKeyID, "identity", ak.UserName)
			gw.RateLimiter.RecordSuccess(clientIP)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
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
