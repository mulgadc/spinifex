package awsgw

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	ekscluster "github.com/mulgadc/spinifex/spinifex/domains/eks/cluster"
)

// OIDCPublisher serves the unauthenticated per-cluster OIDC discovery document
// and JWKS (IRSA). The EKS domain owns the records they are built from; the
// route only maps onto the account and cluster.
type OIDCPublisher interface {
	ServeDiscoveryDocument(w http.ResponseWriter, r *http.Request, accountID, clusterName string)
	ServeJWKS(w http.ResponseWriter, r *http.Request, accountID, clusterName string)
}

var _ OIDCPublisher = (*ekscluster.OIDCPublisher)(nil)

// PublicRoutes mounts the routes served ahead of SigV4 and the throttle: the
// OIDC discovery endpoints (IRSA) and the OCI registry at /v2. The registry
// authenticates by its own token rather than SigV4, so it is not part of the
// AWS seam. Only the exact /v2 subtree is the registry.
func PublicRoutes(oidc OIDCPublisher, registry http.Handler) func(chi.Router) {
	return func(r chi.Router) {
		r.Group(func(pub chi.Router) {
			pub.Get("/oidc/eks/{region}/{accountID}/{clusterName}/.well-known/openid-configuration", oidcDiscoveryDocument(oidc))
			pub.Get("/oidc/eks/{region}/{accountID}/{clusterName}/keys", oidcJWKS(oidc))
		})
		if registry == nil {
			registry = http.HandlerFunc(registryNotConfigured)
		}
		r.Mount("/v2", registry)
	}
}

func oidcDiscoveryDocument(p OIDCPublisher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if p == nil {
			slog.Error("OIDC discovery: publisher not configured")
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		p.ServeDiscoveryDocument(w, r, chi.URLParam(r, "accountID"), chi.URLParam(r, "clusterName"))
	}
}

func oidcJWKS(p OIDCPublisher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if p == nil {
			slog.Error("OIDC JWKS: publisher not configured")
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		p.ServeJWKS(w, r, chi.URLParam(r, "accountID"), chi.URLParam(r, "clusterName"))
	}
}

func registryNotConfigured(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotImplemented)
	_, _ = w.Write([]byte(`{"errors":[{"code":"UNSUPPORTED","message":"registry not configured"}]}`))
}
