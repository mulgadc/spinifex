package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	gateway_ecrauth "github.com/mulgadc/spinifex/spinifex/gateway/ecrauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func decodeTokenResp(t *testing.T, w *httptest.ResponseRecorder) ociTokenResponse {
	t.Helper()
	var resp ociTokenResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return resp
}

func TestECRToken_ExchangesBasicForBearer(t *testing.T) {
	iss, verify := newECRAuth(t)
	gw := &GatewayConfig{
		Region: ecrTestRegion, InternalSuffix: ecrTestSuffix, ECRTokenIssuer: iss, ECRTokenVerifier: verify,
		IAMService: ecrBridgeTestIAM(ecrTestAccount),
	}

	req := httptest.NewRequest(http.MethodGet, "/v2/token", nil)
	req.Header.Set("Authorization", mintBasic(t, iss, ecrTestAccount))
	w := httptest.NewRecorder()
	gw.handleECRToken(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	resp := decodeTokenResp(t, w)
	require.NotEmpty(t, resp.Token)
	assert.Equal(t, resp.Token, resp.AccessToken)
	assert.Positive(t, resp.ExpiresIn)

	// The returned token must verify and carry the original account.
	claims, err := verify.Verify(resp.Token)
	require.NoError(t, err)
	assert.Equal(t, ecrTestAccount, claims.AccountID)
}

func TestECRToken_BearerCredentialAccepted(t *testing.T) {
	iss, verify := newECRAuth(t)
	gw := &GatewayConfig{
		Region: ecrTestRegion, InternalSuffix: ecrTestSuffix, ECRTokenIssuer: iss, ECRTokenVerifier: verify,
		IAMService: ecrBridgeTestIAM(ecrTestAccount),
	}

	tok, _, err := iss.Mint(gateway_ecrauth.Principal{
		AccountID:   ecrTestAccount,
		ARN:         "arn:aws:iam::" + ecrTestAccount + ":user/dev",
		Type:        principalTypeUser,
		AccessKeyID: ecrBridgeTestAKID,
	})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/v2/token", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	gw.handleECRToken(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.NotEmpty(t, decodeTokenResp(t, w).Token)
}

func TestECRToken_NoCredentialChallenges401(t *testing.T) {
	iss, verify := newECRAuth(t)
	gw := &GatewayConfig{Region: ecrTestRegion, InternalSuffix: ecrTestSuffix, ECRTokenIssuer: iss, ECRTokenVerifier: verify}

	req := httptest.NewRequest(http.MethodGet, "/v2/token", nil)
	w := httptest.NewRecorder()
	gw.handleECRToken(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	// Must not re-advertise Bearer here, or clients loop on the token endpoint.
	assert.Empty(t, w.Header().Get("WWW-Authenticate"))
}

func TestECRToken_InvalidCredential401(t *testing.T) {
	iss, verify := newECRAuth(t)
	gw := &GatewayConfig{Region: ecrTestRegion, InternalSuffix: ecrTestSuffix, ECRTokenIssuer: iss, ECRTokenVerifier: verify}

	req := httptest.NewRequest(http.MethodGet, "/v2/token", nil)
	req.Header.Set("Authorization", "Bearer not.a.realtoken")
	w := httptest.NewRecorder()
	gw.handleECRToken(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestECRToken_CrossAccountForbidden(t *testing.T) {
	iss, verify := newECRAuth(t)
	gw := &GatewayConfig{Region: ecrTestRegion, InternalSuffix: ecrTestSuffix, ECRTokenIssuer: iss, ECRTokenVerifier: verify}

	req := httptest.NewRequest(http.MethodGet, "/v2/token", nil)
	req.Header.Set("Authorization", mintBasic(t, iss, ecrTestAccount))
	ctx := context.WithValue(req.Context(), ctxTargetAccount, "999999999999")
	w := httptest.NewRecorder()
	gw.handleECRToken(w, req.WithContext(ctx))

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestECRToken_MultipleAuthHeadersRejected(t *testing.T) {
	iss, verify := newECRAuth(t)
	gw := &GatewayConfig{Region: ecrTestRegion, InternalSuffix: ecrTestSuffix, ECRTokenIssuer: iss, ECRTokenVerifier: verify}

	req := httptest.NewRequest(http.MethodGet, "/v2/token", nil)
	req.Header.Add("Authorization", "Bearer a.b.c")
	req.Header.Add("Authorization", "Bearer d.e.f")
	w := httptest.NewRecorder()
	gw.handleECRToken(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestOCIRegistry_TokenRouted proves chi routes GET /v2/token to the token
// endpoint (200) rather than the bridge-guarded /* catch-all.
func TestOCIRegistry_TokenRouted(t *testing.T) {
	iss, verify := newECRAuth(t)
	gw := &GatewayConfig{
		DisableLogging: true, Region: ecrTestRegion, InternalSuffix: ecrTestSuffix, ECRTokenIssuer: iss, ECRTokenVerifier: verify,
		IAMService: ecrBridgeTestIAM(ecrTestAccount),
	}
	r := chi.NewRouter()
	gw.mountOCIRegistry(r)

	req := httptest.NewRequest(http.MethodGet, "/v2/token", nil)
	req.Host = ecrTestAccount + ".dkr.ecr." + ecrTestRegion + "." + ecrTestSuffix
	req.Header.Set("Authorization", mintBasic(t, iss, ecrTestAccount))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.NotEmpty(t, decodeTokenResp(t, w).Token)
}

func TestECRToken_NotConfigured501(t *testing.T) {
	gw := &GatewayConfig{Region: ecrTestRegion, InternalSuffix: ecrTestSuffix}
	req := httptest.NewRequest(http.MethodGet, "/v2/token", nil)
	w := httptest.NewRecorder()
	gw.handleECRToken(w, req)

	assert.Equal(t, http.StatusNotImplemented, w.Code)
}
