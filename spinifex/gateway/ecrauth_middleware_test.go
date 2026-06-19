package gateway

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	gateway_ecrauth "github.com/mulgadc/spinifex/spinifex/gateway/ecrauth"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	ecrTestRegion   = "ap-southeast-2"
	ecrTestSuffix   = "mulga.internal"
	ecrTestAudience = "ecr." + ecrTestRegion + "." + ecrTestSuffix
	ecrTestAccount  = "000000000001"
)

func ecrTestMasterKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	_, err := rand.Read(k)
	require.NoError(t, err)
	return k
}

// newECRAuth builds a wired issuer+verifier over a freshly generated signing key.
func newECRAuth(t *testing.T) (*gateway_ecrauth.Issuer, *gateway_ecrauth.Verifier) {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	key, verify, err := gateway_ecrauth.LoadOrCreateSigningKey(js, ecrTestMasterKey(t), 1)
	require.NoError(t, err)
	return gateway_ecrauth.NewIssuer(key, ecrTestAudience), gateway_ecrauth.NewVerifier(verify, ecrTestAudience)
}

func mintBasic(t *testing.T, iss *gateway_ecrauth.Issuer, account string) string {
	t.Helper()
	tok, _, err := iss.Mint(gateway_ecrauth.Principal{AccountID: account, ARN: "arn:aws:iam::" + account + ":user/dev"})
	require.NoError(t, err)
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("AWS:"+tok))
}

func okHandler() (http.Handler, *bool) {
	called := new(bool)
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*called = true
		w.WriteHeader(http.StatusOK)
	})
	return h, called
}

func TestExtractECRToken(t *testing.T) {
	good := base64.StdEncoding.EncodeToString([]byte("AWS:abc.def.ghi"))
	cases := []struct {
		name  string
		authz string
		want  string
		ok    bool
	}{
		{"bearer", "Bearer abc.def.ghi", "abc.def.ghi", true},
		{"basic AWS", "Basic " + good, "abc.def.ghi", true},
		{"basic wrong user", "Basic " + base64.StdEncoding.EncodeToString([]byte("root:x")), "", false},
		{"basic empty pass", "Basic " + base64.StdEncoding.EncodeToString([]byte("AWS:")), "", false},
		{"basic bad base64", "Basic !!!notbase64", "", false},
		{"unknown scheme", "AWS4-HMAC-SHA256 cred", "", false},
		{"empty", "", "", false},
		{"bearer empty", "Bearer ", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := extractECRToken(c.authz)
			assert.Equal(t, c.ok, ok)
			assert.Equal(t, c.want, got)
		})
	}
}

func TestECRAuthBridge_NilVerifierPassesThrough(t *testing.T) {
	gw := &GatewayConfig{}
	next, called := okHandler()
	w := httptest.NewRecorder()
	gw.ecrAuthBridge(next).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v2/", nil))
	assert.True(t, *called)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestECRAuthBridge_NoAuthChallenges401(t *testing.T) {
	_, verify := newECRAuth(t)
	gw := &GatewayConfig{Region: ecrTestRegion, InternalSuffix: ecrTestSuffix, ECRTokenVerifier: verify}
	next, called := okHandler()

	req := httptest.NewRequest(http.MethodGet, "/v2/", nil)
	req.Host = ecrTestAccount + ".dkr.ecr." + ecrTestRegion + "." + ecrTestSuffix
	w := httptest.NewRecorder()
	gw.ecrAuthBridge(next).ServeHTTP(w, req)

	assert.False(t, *called, "handler must not run unauthenticated")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	// Bearer challenge points clients at the /v2/token endpoint; the bridge still
	// accepts Basic AWS:<jwt> directly.
	assert.Equal(t,
		`Bearer realm="https://`+req.Host+`/v2/token",service="`+req.Host+`"`,
		w.Header().Get("WWW-Authenticate"))
}

func TestECRAuthBridge_ValidTokenPassesThrough(t *testing.T) {
	iss, verify := newECRAuth(t)
	gw := &GatewayConfig{Region: ecrTestRegion, InternalSuffix: ecrTestSuffix, ECRTokenVerifier: verify}
	next, called := okHandler()

	req := httptest.NewRequest(http.MethodGet, "/v2/", nil)
	req.Header.Set("Authorization", mintBasic(t, iss, ecrTestAccount))
	w := httptest.NewRecorder()
	gw.ecrAuthBridge(next).ServeHTTP(w, req)

	assert.True(t, *called)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestECRAuthBridge_CrossAccountForbidden(t *testing.T) {
	iss, verify := newECRAuth(t)
	gw := &GatewayConfig{Region: ecrTestRegion, InternalSuffix: ecrTestSuffix, ECRTokenVerifier: verify}
	next, called := okHandler()

	// Token is for ecrTestAccount, but the host targets a different account.
	req := httptest.NewRequest(http.MethodGet, "/v2/", nil)
	req.Header.Set("Authorization", mintBasic(t, iss, ecrTestAccount))
	ctx := context.WithValue(req.Context(), ctxTargetAccount, "999999999999")
	w := httptest.NewRecorder()
	gw.ecrAuthBridge(next).ServeHTTP(w, req.WithContext(ctx))

	assert.False(t, *called)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestECRAuthBridge_MultipleAuthHeadersRejected(t *testing.T) {
	_, verify := newECRAuth(t)
	gw := &GatewayConfig{Region: ecrTestRegion, InternalSuffix: ecrTestSuffix, ECRTokenVerifier: verify}
	next, called := okHandler()

	req := httptest.NewRequest(http.MethodGet, "/v2/", nil)
	req.Header.Add("Authorization", "Bearer a.b.c")
	req.Header.Add("Authorization", "Bearer d.e.f")
	w := httptest.NewRecorder()
	gw.ecrAuthBridge(next).ServeHTTP(w, req)

	assert.False(t, *called)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestECRAuthBridge_InvalidTokenChallenges401(t *testing.T) {
	_, verify := newECRAuth(t)
	gw := &GatewayConfig{Region: ecrTestRegion, InternalSuffix: ecrTestSuffix, ECRTokenVerifier: verify}
	next, called := okHandler()

	req := httptest.NewRequest(http.MethodGet, "/v2/", nil)
	req.Header.Set("Authorization", "Bearer not.a.realtoken")
	w := httptest.NewRecorder()
	gw.ecrAuthBridge(next).ServeHTTP(w, req)

	assert.False(t, *called)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}
