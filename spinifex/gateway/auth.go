package gateway

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mulgadc/predastore/auth"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
)

const (
	// Maximum allowed clock skew for signature validation (5 minutes)
	maxClockSkew = 5 * time.Minute

	// Maximum request body size for signature validation (10 MB)
	maxBodySize = 10 * 1024 * 1024
)

// SigV4AuthMiddleware returns stdlib middleware that validates AWS Signature V4 authentication.
func (gw *GatewayConfig) SigV4AuthMiddleware() func(http.Handler) http.Handler {
	if gw.RateLimiter == nil {
		gw.RateLimiter = NewAuthRateLimiter()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Rate-limit check: reject locked-out IPs before any crypto work.
			clientIP := extractClientIP(r)
			if errCode := gw.RateLimiter.CheckIP(clientIP); errCode != "" {
				gw.writeSigV4Error(w, r, errCode)
				return
			}

			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				gw.writeSigV4Error(w, r, awserrors.ErrorMissingAuthenticationToken)
				return
			}

			// Parse the Authorization header
			// Format: AWS4-HMAC-SHA256 Credential=ACCESS_KEY/DATE/REGION/SERVICE/aws4_request, SignedHeaders=..., Signature=...
			if !strings.HasPrefix(authHeader, "AWS4-HMAC-SHA256 ") {
				gw.writeSigV4Error(w, r, awserrors.ErrorIncompleteSignature)
				return
			}

			// Extract components from the Authorization header
			parts := strings.Split(authHeader[len("AWS4-HMAC-SHA256 "):], ", ")
			if len(parts) != 3 {
				gw.writeSigV4Error(w, r, awserrors.ErrorIncompleteSignature)
				return
			}

			// Parse Credential
			var accessKey, date, region, service string
			if after, ok := strings.CutPrefix(parts[0], "Credential="); ok {
				credParts := strings.Split(after, "/")
				if len(credParts) != 5 || credParts[4] != "aws4_request" {
					gw.writeSigV4Error(w, r, awserrors.ErrorIncompleteSignature)
					return
				}
				accessKey = credParts[0]
				date = credParts[1]
				region = credParts[2]
				service = credParts[3]
			} else {
				gw.writeSigV4Error(w, r, awserrors.ErrorIncompleteSignature)
				return
			}

			// Parse SignedHeaders
			var signedHeaders string
			if after, ok := strings.CutPrefix(parts[1], "SignedHeaders="); ok {
				signedHeaders = after
			} else {
				gw.writeSigV4Error(w, r, awserrors.ErrorIncompleteSignature)
				return
			}

			// Enforce that host and x-amz-date are signed. AWS SDKs always sign
			// both; omitting either lets a captured Authorization header replay
			// against a different vhost or outside the X-Amz-Date skew window.
			// Mirrors predastore/auth.RequireSignedHeaders.
			if !signedHeadersIncludeHostAndDate(signedHeaders) {
				gw.writeSigV4Error(w, r, awserrors.ErrorIncompleteSignature)
				return
			}

			// Parse Signature
			var providedSignature string
			if after, ok := strings.CutPrefix(parts[2], "Signature="); ok {
				providedSignature = after
			} else {
				gw.writeSigV4Error(w, r, awserrors.ErrorIncompleteSignature)
				return
			}

			// Fail fast when NATS is down. IAMService.LookupAccessKey runs against
			// JetStream KV and waits 5s per attempt before context.DeadlineExceeded;
			// the catch-all IsConnected check in gateway.Request() is unreachable
			// because this middleware runs first. Mirror it here so authenticated
			// paths return the cluster-unavailable 503 promised by 1c. Only fires
			// when NATSConn is set but disconnected — production always assigns
			// the conn in awsgw.launchService; test helpers leaving it nil get
			// the existing pre-1c behaviour.
			if gw.NATSConn != nil && !gw.NATSConn.IsConnected() {
				gw.writeClusterUnavailable(w, r, service)
				return
			}

			// Lookup access key in IAM KV store
			if gw.IAMService == nil {
				slog.Error("SigV4 auth: IAM service not initialized")
				gw.writeSigV4Error(w, r, awserrors.ErrorInternalError)
				return
			}

			ak, err := gw.IAMService.LookupAccessKey(accessKey)
			if err != nil {
				if strings.Contains(err.Error(), awserrors.ErrorIAMNoSuchEntity) {
					slog.Warn("Auth failure: access key not found", "accessKeyID", accessKey, "sourceIP", clientIP)
					gw.RateLimiter.RecordFailure(clientIP)
					gw.writeSigV4Error(w, r, awserrors.ErrorInvalidClientTokenId)
					return
				}
				slog.Error("IAM lookup failed", "accessKeyID", accessKey, "err", err)
				gw.writeSigV4Error(w, r, awserrors.ErrorInternalError)
				return
			}
			if ak.Status != handlers_iam.AccessKeyStatusActive {
				slog.Warn("Auth failure: access key inactive", "accessKeyID", accessKey, "sourceIP", clientIP)
				gw.RateLimiter.RecordFailure(clientIP)
				gw.writeSigV4Error(w, r, awserrors.ErrorInvalidClientTokenId)
				return
			}

			secret, err := gw.IAMService.DecryptSecret(ak.SecretAccessKey)
			if err != nil {
				slog.Error("Failed to decrypt IAM secret", "accessKeyID", accessKey, "err", err)
				gw.writeSigV4Error(w, r, awserrors.ErrorInternalError)
				return
			}

			// Get timestamp from X-Amz-Date header
			timestamp := r.Header.Get("X-Amz-Date")
			if timestamp == "" {
				gw.writeSigV4Error(w, r, awserrors.ErrorIncompleteSignature)
				return
			}

			// Validate timestamp is within acceptable bounds to prevent replay attacks
			parsedTime, err := time.Parse("20060102T150405Z", timestamp)
			if err != nil {
				slog.Debug("Invalid timestamp format", "timestamp", timestamp)
				gw.writeSigV4Error(w, r, awserrors.ErrorIncompleteSignature)
				return
			}
			if time.Since(parsedTime).Abs() > maxClockSkew {
				slog.Debug("Signature expired", "timestamp", timestamp, "skew", time.Since(parsedTime))
				gw.writeSigV4Error(w, r, awserrors.ErrorSignatureDoesNotMatch)
				return
			}

			// Limit request body size to prevent OOM from unauthenticated requests
			r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)

			// Read body once and re-buffer for downstream handlers
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

			// Compute expected signature using decrypted secret
			expectedSignature := computeSignatureWithSecret(r, body, secret, date, timestamp, region, service, signedHeaders)

			// Compare signatures using constant-time comparison to prevent timing attacks
			if subtle.ConstantTimeCompare([]byte(expectedSignature), []byte(providedSignature)) != 1 {
				slog.Warn("Auth failure: signature mismatch",
					"accessKeyID", accessKey,
					"sourceIP", clientIP,
				)
				gw.RateLimiter.RecordFailure(clientIP)
				gw.writeSigV4Error(w, r, awserrors.ErrorSignatureDoesNotMatch)
				return
			}

			// Store parsed auth data in context for downstream handlers
			ctx := r.Context()
			ctx = context.WithValue(ctx, ctxIdentity, ak.UserName)
			ctx = context.WithValue(ctx, ctxAccountID, ak.AccountID)
			ctx = context.WithValue(ctx, ctxService, service)
			ctx = context.WithValue(ctx, ctxRegion, region)
			ctx = context.WithValue(ctx, ctxAccessKey, accessKey)

			// Parse once; dispatchers reuse via ctxQueryArgs. On error, the
			// dispatcher re-parses and returns MalformedQueryString.
			if args, err := ParseAWSQueryArgs(string(body)); err == nil {
				ctx = context.WithValue(ctx, ctxQueryArgs, args)
				if action := args["Action"]; action != "" {
					ctx = context.WithValue(ctx, ctxAction, action)
				}
			}

			slog.Debug("SigV4 authentication successful", "accessKey", accessKey, "identity", ak.UserName)
			gw.RateLimiter.RecordSuccess(clientIP)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// computeSignatureWithSecret builds the canonical request and computes the AWS Signature V4 signature
// using the provided secret key. The body is passed explicitly since it has already been read from r.Body.
func computeSignatureWithSecret(r *http.Request, body []byte, secretKey, date, timestamp, region, service, signedHeaders string) string {
	// Build canonical URI (URI-encoded path)
	path := r.URL.Path
	if path == "" {
		path = "/"
	}
	canonicalURI := auth.UriEncode(path, false)
	if canonicalURI == "" {
		canonicalURI = "/"
	}

	// Build canonical query string (sorted, encoded)
	canonicalQueryString := buildCanonicalQueryString(r.URL.RawQuery)

	// Build canonical headers from SignedHeaders list
	headersList := strings.Split(signedHeaders, ";")
	sort.Strings(headersList)

	var canonicalHeaders strings.Builder
	for _, header := range headersList {
		header = strings.ToLower(strings.TrimSpace(header))
		var value string
		if header == "host" {
			value = r.Host
		} else {
			value = r.Header.Get(canonicalHeaderName(header))
		}
		// Trim leading/trailing whitespace and collapse multiple spaces
		value = strings.TrimSpace(value)
		canonicalHeaders.WriteString(header)
		canonicalHeaders.WriteString(":")
		canonicalHeaders.WriteString(value)
		canonicalHeaders.WriteString("\n")
	}

	// Hash payload body with SHA256
	payloadHash := auth.HashSHA256(string(body))

	// Build canonical request
	canonicalRequest := fmt.Sprintf(
		"%s\n%s\n%s\n%s\n%s\n%s",
		r.Method,
		canonicalURI,
		canonicalQueryString,
		canonicalHeaders.String(),
		signedHeaders,
		payloadHash,
	)

	hashedCanonicalRequest := auth.HashSHA256(canonicalRequest)

	// Build string-to-sign
	scope := fmt.Sprintf("%s/%s/%s/aws4_request", date, region, service)
	stringToSign := fmt.Sprintf(
		"AWS4-HMAC-SHA256\n%s\n%s\n%s",
		timestamp,
		scope,
		hashedCanonicalRequest,
	)

	// Derive signing key and compute signature
	signingKey := auth.GetSigningKey(secretKey, date, region, service)
	signature := auth.HmacSHA256Hex(signingKey, stringToSign)

	return signature
}

// buildCanonicalQueryString creates the canonical query string according to AWS specs.
func buildCanonicalQueryString(queryString string) string {
	if queryString == "" {
		return ""
	}

	// Parse query parameters
	params := make(map[string][]string)
	pairs := strings.SplitSeq(queryString, "&")
	for pair := range pairs {
		if pair == "" {
			continue
		}
		kv := strings.SplitN(pair, "=", 2)
		key := kv[0]
		value := ""
		if len(kv) == 2 {
			value = kv[1]
		}
		params[key] = append(params[key], value)
	}

	// Sort keys
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Build canonical query string
	var result []string
	for _, key := range keys {
		values := params[key]
		sort.Strings(values)
		encodedKey := auth.UriEncode(key, true)
		for _, v := range values {
			encodedValue := auth.UriEncode(v, true)
			result = append(result, fmt.Sprintf("%s=%s", encodedKey, encodedValue))
		}
	}

	return strings.Join(result, "&")
}

// signedHeadersIncludeHostAndDate reports whether the SigV4 SignedHeaders
// list (";"-separated) includes both "host" and "x-amz-date".
func signedHeadersIncludeHostAndDate(signedHeaders string) bool {
	var hasHost, hasDate bool
	for h := range strings.SplitSeq(signedHeaders, ";") {
		switch strings.ToLower(strings.TrimSpace(h)) {
		case "host":
			hasHost = true
		case "x-amz-date":
			hasDate = true
		}
	}
	return hasHost && hasDate
}

// canonicalHeaderName converts a lowercase header name to the canonical form for lookup.
func canonicalHeaderName(header string) string {
	// Convert header names like "x-amz-date" to "X-Amz-Date" for http.Header.Get
	parts := strings.Split(header, "-")
	for i, part := range parts {
		if len(part) > 0 {
			parts[i] = strings.ToUpper(part[:1]) + part[1:]
		}
	}
	return strings.Join(parts, "-")
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
