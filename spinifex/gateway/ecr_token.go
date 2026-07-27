package gateway

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	gateway_ecr "github.com/mulgadc/spinifex/spinifex/gateway/ecr"
	gateway_ecrauth "github.com/mulgadc/spinifex/spinifex/gateway/ecrauth"
)

// ociTokenResponse is the Docker Registry v2 token-endpoint body. token and
// access_token carry the same JWT for compatibility across client versions.
type ociTokenResponse struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	IssuedAt    string `json:"issued_at"`
}

// handleECRToken implements the OCI Bearer token endpoint advertised by the
// /v2 401 challenge. The caller presents its existing credential (Basic
// AWS:<jwt> from GetAuthorizationToken, or Bearer <jwt>); this verifies it,
// enforces the registry-host account match, and re-mints a fresh full-TTL
// token. Re-minting on each call is the token-refresh path: a client near
// expiry obtains a new token without re-running SigV4.
//
// The endpoint authenticates the presented credential itself, so it mounts
// outside the bearer auth bridge. A missing or invalid credential returns 401
// without a Bearer realm to avoid a challenge loop.
func (gw *GatewayConfig) handleECRToken(w http.ResponseWriter, r *http.Request) {
	if gw.ECRTokenVerifier == nil || gw.ECRTokenIssuer == nil {
		gateway_ecr.WriteError(w, http.StatusNotImplemented, "UNSUPPORTED", "token endpoint not configured")
		return
	}

	authz := r.Header.Values("Authorization")
	if len(authz) > 1 {
		gateway_ecr.WriteError(w, http.StatusBadRequest, "UNAUTHORIZED", "multiple Authorization headers")
		return
	}
	raw := ""
	if len(authz) == 1 {
		raw = authz[0]
	}
	token, ok := extractECRToken(raw)
	if !ok {
		gateway_ecr.WriteError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	claims, err := gw.ECRTokenVerifier.Verify(token)
	if err != nil {
		slog.Debug("ECR token endpoint: verify failed", "err", err)
		gateway_ecr.WriteError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid credentials")
		return
	}

	// Cross-account guard mirrors the auth bridge: a token is account-scoped and
	// must match the account in the registry host it is presented against.
	if target, _ := r.Context().Value(ctxTargetAccount).(string); target != "" && target != claims.AccountID {
		gateway_ecr.WriteError(w, http.StatusForbidden, "DENIED", "token account does not match registry host")
		return
	}

	// A refresh must not re-issue a token for an identity that was revoked
	// since the presented token was minted: rehydrate against current IAM/STS
	// state and mint the replacement from that authoritative record, never
	// blindly from the presented claims.
	principal, err := gw.resolveECRPrincipal(claims)
	if err != nil {
		// No Bearer challenge here: this is already the challenge target, and a
		// revoked identity retrying the challenge flow would only loop back.
		if isECRDependencyFailure(err) {
			slog.Error("ECR token endpoint: principal rehydration dependency failure", "err", err)
			gateway_ecr.WriteError(w, http.StatusServiceUnavailable, "UNKNOWN", "authorization unavailable")
			return
		}
		slog.Warn("ECR token endpoint: refusing to refresh revoked identity", "err", err)
		gateway_ecr.WriteError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid credentials")
		return
	}

	callerARN, err := buildCallerARN(principal.accountID, principal.identity, principal.principalType, principal.assumedRoleARN)
	if err != nil {
		slog.Error("ECR token endpoint: cannot build canonical caller ARN", "err", err)
		gateway_ecr.WriteError(w, http.StatusInternalServerError, "SERVER_ERROR", "token mint failed")
		return
	}

	fresh, expiresAt, err := gw.ECRTokenIssuer.Mint(gateway_ecrauth.Principal{
		AccountID:   principal.accountID,
		ARN:         callerARN,
		Type:        principal.principalType,
		AccessKeyID: claims.AccessKeyID,
	})
	if err != nil {
		slog.Error("ECR token endpoint: mint failed", "err", err)
		gateway_ecr.WriteError(w, http.StatusInternalServerError, "SERVER_ERROR", "token mint failed")
		return
	}

	body, err := json.Marshal(ociTokenResponse{
		Token:       fresh,
		AccessToken: fresh,
		ExpiresIn:   int(time.Until(expiresAt).Seconds()),
		IssuedAt:    time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		slog.Error("ECR token endpoint: marshal failed", "err", err)
		gateway_ecr.WriteError(w, http.StatusInternalServerError, "SERVER_ERROR", "response encode failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		slog.Error("ECR token endpoint: write failed", "err", err)
	}
}
