package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	"github.com/mulgadc/spinifex/spinifex/testutil"
)

const (
	testDiscAccount = "000000000001"
	testDiscCluster = "h1"
	testDiscIssuer  = "https://10.0.0.1:9999/oidc/eks/ap-southeast-2/000000000001/h1"
)

// seedDiscoveryCluster writes a ClusterMeta (with OIDCIssuer) and a JWKS doc
// into the per-account EKS bucket, returning a router wired to the same NATS.
func seedDiscoveryCluster(t *testing.T) http.Handler {
	t.Helper()
	_, nc, js := testutil.StartTestJetStream(t)

	kv, err := handlers_eks.GetOrCreateAccountBucket(js, testDiscAccount, 1)
	if err != nil {
		t.Fatalf("account bucket: %v", err)
	}
	if err := handlers_eks.PutClusterMeta(kv, &handlers_eks.ClusterMeta{
		Name:       testDiscCluster,
		Status:     handlers_eks.ClusterStatusActive,
		OIDCIssuer: testDiscIssuer,
	}); err != nil {
		t.Fatalf("put cluster meta: %v", err)
	}
	if _, err := kv.Put(handlers_eks.OIDCJWKSKey(testDiscCluster), []byte(`{"keys":[{"kty":"EC","kid":"abc"}]}`)); err != nil {
		t.Fatalf("put jwks: %v", err)
	}

	gw := &GatewayConfig{DisableLogging: true, NATSConn: nc}
	return gw.SetupRoutes()
}

func TestOIDCDiscoveryDocument(t *testing.T) {
	h := seedDiscoveryCluster(t)

	req := httptest.NewRequest(http.MethodGet,
		"/oidc/eks/ap-southeast-2/"+testDiscAccount+"/"+testDiscCluster+"/.well-known/openid-configuration", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var doc oidcDiscoveryDocument
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.Issuer != testDiscIssuer {
		t.Fatalf("issuer = %q, want %q", doc.Issuer, testDiscIssuer)
	}
	if doc.JWKSURI != testDiscIssuer+"/keys" {
		t.Fatalf("jwks_uri = %q, want issuer+/keys", doc.JWKSURI)
	}
	if len(doc.IDTokenSigningAlgValuesSupported) != 1 || doc.IDTokenSigningAlgValuesSupported[0] != "ES256" {
		t.Fatalf("alg = %v, want [ES256]", doc.IDTokenSigningAlgValuesSupported)
	}
}

func TestOIDCJWKS(t *testing.T) {
	h := seedDiscoveryCluster(t)

	req := httptest.NewRequest(http.MethodGet,
		"/oidc/eks/ap-southeast-2/"+testDiscAccount+"/"+testDiscCluster+"/keys", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
	var jwks struct {
		Keys []map[string]string `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &jwks); err != nil {
		t.Fatalf("unmarshal jwks: %v", err)
	}
	if len(jwks.Keys) != 1 || jwks.Keys[0]["kid"] != "abc" {
		t.Fatalf("jwks = %s", rec.Body.String())
	}
}

func TestOIDCDiscovery_UnknownCluster404(t *testing.T) {
	h := seedDiscoveryCluster(t)

	for _, path := range []string{
		"/oidc/eks/ap-southeast-2/" + testDiscAccount + "/nope/.well-known/openid-configuration",
		"/oidc/eks/ap-southeast-2/" + testDiscAccount + "/nope/keys",
		"/oidc/eks/ap-southeast-2/999999999999/h1/keys",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("path %s: status = %d, want 404", path, rec.Code)
		}
	}
}

// TestOIDCDiscovery_NoAuth confirms the discovery routes bypass SigV4 — they
// return 200 with no Authorization header, unlike the authenticated surface.
func TestOIDCDiscovery_NoAuth(t *testing.T) {
	h := seedDiscoveryCluster(t)
	req := httptest.NewRequest(http.MethodGet,
		"/oidc/eks/ap-southeast-2/"+testDiscAccount+"/"+testDiscCluster+"/keys", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unauthenticated discovery status = %d, want 200", rec.Code)
	}
}
