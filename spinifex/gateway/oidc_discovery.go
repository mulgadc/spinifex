package gateway

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// OIDCPublisher serves the unauthenticated per-cluster OIDC discovery document
// and JWKS (IRSA). The EKS domain owns the records they are built from; the
// gateway only maps the route onto the account and cluster.
type OIDCPublisher interface {
	ServeDiscoveryDocument(w http.ResponseWriter, r *http.Request, accountID, clusterName string)
	ServeJWKS(w http.ResponseWriter, r *http.Request, accountID, clusterName string)
}

// OIDCDiscoveryDocument serves the cluster's OpenID discovery document.
func (gw *GatewayConfig) OIDCDiscoveryDocument(w http.ResponseWriter, r *http.Request) {
	if gw.OIDC == nil {
		slog.Error("OIDC discovery: publisher not configured")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	gw.OIDC.ServeDiscoveryDocument(w, r, chi.URLParam(r, "accountID"), chi.URLParam(r, "clusterName"))
}

// OIDCJWKS serves the cluster's JWKS document as referenced by the discovery
// document's jwks_uri.
func (gw *GatewayConfig) OIDCJWKS(w http.ResponseWriter, r *http.Request) {
	if gw.OIDC == nil {
		slog.Error("OIDC JWKS: publisher not configured")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	gw.OIDC.ServeJWKS(w, r, chi.URLParam(r, "accountID"), chi.URLParam(r, "clusterName"))
}
