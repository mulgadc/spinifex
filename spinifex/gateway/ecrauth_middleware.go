package gateway

import (
	"context"
	"encoding/base64"
	"log/slog"
	"net/http"
	"strings"

	gateway_ecr "github.com/mulgadc/spinifex/spinifex/gateway/ecr"
)

// ecrAuthBridge authenticates /v2/* requests with an ECR token. It accepts a
// single Authorization header carrying "Bearer <jwt>" or "Basic AWS:<jwt>",
// verifies the JWT, enforces that the token account matches the host-derived
// account, and stashes the resolved account/principal on the request context.
// Unauthenticated or invalid requests get a 401 Bearer challenge.
//
// A nil verifier disables the bridge (registry mounts open), matching the nil
// ECRRegistry fallback used by unit tests of unrelated routes.
func (gw *GatewayConfig) ecrAuthBridge(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gw.ECRTokenVerifier == nil {
			next.ServeHTTP(w, r)
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
			gw.writeECRChallenge(w, r)
			return
		}
		claims, err := gw.ECRTokenVerifier.Verify(token)
		if err != nil {
			slog.Debug("ECR auth bridge: token verify failed", "err", err)
			gw.writeECRChallenge(w, r)
			return
		}

		// Cross-account guard: a token is account-scoped, so it must match the
		// account in the registry host it is presented against.
		if target, _ := r.Context().Value(ctxTargetAccount).(string); target != "" && target != claims.AccountID {
			gateway_ecr.WriteError(w, http.StatusForbidden, "DENIED", "token account does not match registry host")
			return
		}

		ctx := gateway_ecr.WithAuthAccount(r.Context(), claims.AccountID)
		ctx = context.WithValue(ctx, ctxAuthPrincipal, claims.Subject)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// extractECRToken pulls the JWT from a single Authorization header value,
// accepting "Bearer <jwt>" or "Basic base64(AWS:<jwt>)". ok is false for any
// other scheme, malformed value, or empty token. SigV4 is not accepted on /v2.
func extractECRToken(authz string) (string, bool) {
	scheme, rest, found := strings.Cut(authz, " ")
	if !found {
		return "", false
	}
	switch {
	case strings.EqualFold(scheme, "Bearer"):
		rest = strings.TrimSpace(rest)
		return rest, rest != ""
	case strings.EqualFold(scheme, "Basic"):
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(rest))
		if err != nil {
			return "", false
		}
		user, pass, found := strings.Cut(string(decoded), ":")
		if !found || user != "AWS" || pass == "" {
			return "", false
		}
		return pass, true
	default:
		return "", false
	}
}

// writeECRChallenge emits the 401 Bearer challenge advertising the /v2/token
// endpoint. OCI clients (docker, crane, skopeo) fetch a token from the realm
// using their stored AWS:<jwt> credential, then replay it as Bearer. The bridge
// still accepts Basic AWS:<jwt> directly, so a Basic-only caller also works. The
// realm and service use the request Host so the client negotiates against the
// address it actually dialed.
func (gw *GatewayConfig) writeECRChallenge(w http.ResponseWriter, r *http.Request) {
	realm := "https://" + r.Host + "/v2/token"
	w.Header().Set("WWW-Authenticate", `Bearer realm="`+realm+`",service="`+r.Host+`"`)
	gateway_ecr.WriteError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
}
