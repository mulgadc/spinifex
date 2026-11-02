package cluster

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/nats-io/nats.go/jetstream"
)

// OIDCDiscoveryDocument is the subset of the OpenID Connect Discovery metadata
// (RFC 8414) that AWS STS and the AWS SDKs consume to validate IRSA
// ServiceAccount tokens. Served unauthenticated at the per-cluster issuer URL.
type OIDCDiscoveryDocument struct {
	Issuer                           string   `json:"issuer"`
	JWKSURI                          string   `json:"jwks_uri"`
	ResponseTypesSupported           []string `json:"response_types_supported"`
	SubjectTypesSupported            []string `json:"subject_types_supported"`
	IDTokenSigningAlgValuesSupported []string `json:"id_token_signing_alg_values_supported"`
	ClaimsSupported                  []string `json:"claims_supported"`
}

// OIDCPublisher serves each EKS cluster's OIDC discovery document and JWKS from
// the per-account bucket this package owns. The caller resolves the account and
// cluster from its route and mounts the handlers unauthenticated.
type OIDCPublisher struct {
	js jetstream.JetStream
}

// NewOIDCPublisher constructs an OIDCPublisher over js.
func NewOIDCPublisher(js jetstream.JetStream) *OIDCPublisher {
	return &OIDCPublisher{js: js}
}

// accountKV opens the read-only per-account EKS bucket. It returns
// jetstream.ErrBucketNotFound (unwrapped) when the account has no clusters,
// which the handlers translate to 404.
func (p *OIDCPublisher) accountKV(r *http.Request, accountID string) (jetstream.KeyValue, error) {
	if p == nil || p.js == nil {
		return nil, errors.New("EKS OIDC publisher JetStream not initialised")
	}
	return p.js.KeyValue(r.Context(), AccountBucketName(accountID))
}

// ServeDiscoveryDocument serves the cluster's OpenID discovery document. The
// `issuer` it returns is the cluster's stored IssuerURL — byte-identical to the
// apiserver's `iss` claim — so an AWS SDK that fetches this document while
// registering the provider sees a matching issuer.
func (p *OIDCPublisher) ServeDiscoveryDocument(w http.ResponseWriter, r *http.Request, accountID, clusterName string) {
	kv, err := p.accountKV(r, accountID)
	if err != nil {
		if errors.Is(err, jetstream.ErrBucketNotFound) {
			http.NotFound(w, r)
			return
		}
		slog.Error("OIDC discovery: open account bucket", "accountID", accountID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	meta, err := GetClusterMeta(r.Context(), kv, clusterName)
	if err != nil || meta == nil || meta.OIDCIssuer == "" {
		http.NotFound(w, r)
		return
	}

	writeOIDCJSON(w, OIDCDiscoveryDocument{
		Issuer:                           meta.OIDCIssuer,
		JWKSURI:                          meta.OIDCIssuer + "/keys",
		ResponseTypesSupported:           []string{"id_token"},
		SubjectTypesSupported:            []string{"public"},
		IDTokenSigningAlgValuesSupported: []string{"ES256"},
		ClaimsSupported:                  []string{"sub", "iss", "aud", "exp", "iat"},
	})
}

// ServeJWKS serves the cluster's JWKS document (the public verification key) as
// referenced by the discovery document's jwks_uri. The stored bytes are written
// as they are, without validating that they parse as a JWKS.
func (p *OIDCPublisher) ServeJWKS(w http.ResponseWriter, r *http.Request, accountID, clusterName string) {
	kv, err := p.accountKV(r, accountID)
	if err != nil {
		if errors.Is(err, jetstream.ErrBucketNotFound) {
			http.NotFound(w, r)
			return
		}
		slog.Error("OIDC JWKS: open account bucket", "accountID", accountID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	entry, err := kv.Get(r.Context(), OIDCJWKSKey(clusterName))
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

func writeOIDCJSON(w http.ResponseWriter, v any) {
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
