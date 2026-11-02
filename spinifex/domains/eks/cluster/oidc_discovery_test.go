package cluster

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	oidcPubAccount = "000000000001"
	oidcPubCluster = "demo"
)

func serveOIDC(t *testing.T, p *OIDCPublisher, jwks bool) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if jwks {
		p.ServeJWKS(rec, req, oidcPubAccount, oidcPubCluster)
	} else {
		p.ServeDiscoveryDocument(rec, req, oidcPubAccount, oidcPubCluster)
	}
	return rec
}

func TestOIDCPublisher_UnwiredAnswers500(t *testing.T) {
	for _, p := range []*OIDCPublisher{nil, NewOIDCPublisher(nil)} {
		assert.Equal(t, http.StatusInternalServerError, serveOIDC(t, p, false).Code)
		assert.Equal(t, http.StatusInternalServerError, serveOIDC(t, p, true).Code)
	}
}

func TestOIDCPublisher_NoAccountBucketAnswers404(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	p := NewOIDCPublisher(testutil.NewJetStream(t, nc))

	assert.Equal(t, http.StatusNotFound, serveOIDC(t, p, false).Code)
	assert.Equal(t, http.StatusNotFound, serveOIDC(t, p, true).Code)
}

// A stored JWKS is written back as it is, even when it is not valid JSON.
func TestOIDCPublisher_ServesStoredJWKSVerbatim(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	kv, err := GetOrCreateAccountBucket(t.Context(), js, oidcPubAccount)
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), OIDCJWKSKey(oidcPubCluster), []byte("not json"))
	require.NoError(t, err)

	rec := serveOIDC(t, NewOIDCPublisher(js), true)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "not json", rec.Body.String())
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.Equal(t, "public, max-age=300", rec.Header().Get("Cache-Control"))
}
