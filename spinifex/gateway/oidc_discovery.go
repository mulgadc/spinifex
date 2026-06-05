package gateway

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	"github.com/nats-io/nats.go"
)

// oidcDiscoveryDocument is the subset of the OpenID Connect Discovery metadata
// (RFC 8414) that AWS STS and the AWS SDKs consume to validate IRSA
// ServiceAccount tokens. Served unauthenticated at the per-cluster issuer URL.
type oidcDiscoveryDocument struct {
	Issuer                           string   `json:"issuer"`
	JWKSURI                          string   `json:"jwks_uri"`
	ResponseTypesSupported           []string `json:"response_types_supported"`
	SubjectTypesSupported            []string `json:"subject_types_supported"`
	IDTokenSigningAlgValuesSupported []string `json:"id_token_signing_alg_values_supported"`
	ClaimsSupported                  []string `json:"claims_supported"`
}

// eksAccountKV opens the read-only per-account EKS bucket. Returns
// nats.ErrBucketNotFound (unwrapped) when the account has no clusters, which
// the OIDC handlers translate to 404.
func (gw *GatewayConfig) eksAccountKV(accountID string) (nats.KeyValue, error) {
	if gw.NATSConn == nil {
		return nil, errors.New("gateway NATS connection not initialised")
	}
	js, err := gw.NATSConn.JetStream()
	if err != nil {
		return nil, err
	}
	return js.KeyValue(handlers_eks.AccountBucketName(accountID))
}

// OIDCDiscoveryDocument serves the cluster's OpenID discovery document. The
// `issuer` it returns is the cluster's stored IssuerURL — byte-identical to the
// apiserver's `iss` claim — so an AWS SDK that fetches this document while
// registering the provider sees a matching issuer.
func (gw *GatewayConfig) OIDCDiscoveryDocument(w http.ResponseWriter, r *http.Request) {
	accountID := chi.URLParam(r, "accountID")
	clusterName := chi.URLParam(r, "clusterName")

	kv, err := gw.eksAccountKV(accountID)
	if err != nil {
		if errors.Is(err, nats.ErrBucketNotFound) {
			http.NotFound(w, r)
			return
		}
		slog.Error("OIDC discovery: open account bucket", "accountID", accountID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	meta, err := handlers_eks.GetClusterMeta(kv, clusterName)
	if err != nil || meta == nil || meta.OIDCIssuer == "" {
		http.NotFound(w, r)
		return
	}

	gw.writeOIDCJSON(w, oidcDiscoveryDocument{
		Issuer:                           meta.OIDCIssuer,
		JWKSURI:                          meta.OIDCIssuer + "/keys",
		ResponseTypesSupported:           []string{"id_token"},
		SubjectTypesSupported:            []string{"public"},
		IDTokenSigningAlgValuesSupported: []string{"ES256"},
		ClaimsSupported:                  []string{"sub", "iss", "aud", "exp", "iat"},
	})
}

// OIDCJWKS serves the cluster's JWKS document (the public verification key) as
// referenced by the discovery document's jwks_uri.
func (gw *GatewayConfig) OIDCJWKS(w http.ResponseWriter, r *http.Request) {
	accountID := chi.URLParam(r, "accountID")
	clusterName := chi.URLParam(r, "clusterName")

	kv, err := gw.eksAccountKV(accountID)
	if err != nil {
		if errors.Is(err, nats.ErrBucketNotFound) {
			http.NotFound(w, r)
			return
		}
		slog.Error("OIDC JWKS: open account bucket", "accountID", accountID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	entry, err := kv.Get(handlers_eks.OIDCJWKSKey(clusterName))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	if _, err := w.Write(entry.Value()); err != nil {
		slog.Debug("OIDC JWKS: write response", "err", err)
	}
}

func (gw *GatewayConfig) writeOIDCJSON(w http.ResponseWriter, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	if _, err := w.Write(body); err != nil {
		slog.Debug("OIDC discovery: write response", "err", err)
	}
}
