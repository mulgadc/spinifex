package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandleGetAuthorizationToken_MintsUsableToken(t *testing.T) {
	iss, verify := newECRAuth(t)
	gw := &GatewayConfig{
		Region: ecrTestRegion, InternalSuffix: ecrTestSuffix,
		ECRTokenIssuer: iss, ECRTokenVerifier: verify, DisableLogging: true,
	}

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
	ctx := context.WithValue(req.Context(), ctxAccountID, ecrTestAccount)
	ctx = context.WithValue(ctx, ctxPrincipalType, principalTypeUser)
	w := httptest.NewRecorder()

	require.NoError(t, gw.handleGetAuthorizationToken(w, req.WithContext(ctx)))
	require.Equal(t, http.StatusOK, w.Code)

	var out struct {
		AuthorizationData []struct {
			AuthorizationToken string  `json:"authorizationToken"`
			ProxyEndpoint      string  `json:"proxyEndpoint"`
			ExpiresAt          float64 `json:"expiresAt"`
		} `json:"authorizationData"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.Len(t, out.AuthorizationData, 1)

	ad := out.AuthorizationData[0]
	assert.Equal(t, "https://"+ecrTestAccount+".dkr.ecr."+ecrTestRegion+"."+ecrTestSuffix, ad.ProxyEndpoint)
	assert.Positive(t, ad.ExpiresAt)

	// authorizationToken decodes to "AWS:<jwt>" and the jwt verifies to the account.
	decoded, err := base64.StdEncoding.DecodeString(ad.AuthorizationToken)
	require.NoError(t, err)
	user, jwtStr, found := strings.Cut(string(decoded), ":")
	require.True(t, found)
	assert.Equal(t, "AWS", user)

	claims, err := verify.Verify(jwtStr)
	require.NoError(t, err)
	assert.Equal(t, ecrTestAccount, claims.AccountID)
}

func TestHandleGetAuthorizationToken_ProxyEndpointCarriesPort(t *testing.T) {
	iss, verify := newECRAuth(t)
	gw := &GatewayConfig{
		Region: ecrTestRegion, InternalSuffix: ecrTestSuffix, RegistryPort: "9999",
		ECRTokenIssuer: iss, ECRTokenVerifier: verify, DisableLogging: true,
	}

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
	ctx := context.WithValue(req.Context(), ctxAccountID, ecrTestAccount)
	ctx = context.WithValue(ctx, ctxPrincipalType, principalTypeUser)
	w := httptest.NewRecorder()
	require.NoError(t, gw.handleGetAuthorizationToken(w, req.WithContext(ctx)))

	var out struct {
		AuthorizationData []struct {
			ProxyEndpoint string `json:"proxyEndpoint"`
		} `json:"authorizationData"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.Len(t, out.AuthorizationData, 1)
	assert.Equal(t, "https://"+ecrTestAccount+".dkr.ecr."+ecrTestRegion+"."+ecrTestSuffix+":9999", out.AuthorizationData[0].ProxyEndpoint)
}

func TestHandleGetAuthorizationToken_NoIssuerNotImplemented(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
	ctx := context.WithValue(req.Context(), ctxAccountID, ecrTestAccount)
	err := gw.handleGetAuthorizationToken(httptest.NewRecorder(), req.WithContext(ctx))
	require.Error(t, err)
}

func TestECRRequest_GetAuthorizationTokenDispatched(t *testing.T) {
	iss, verify := newECRAuth(t)
	gw := &GatewayConfig{
		Region: ecrTestRegion, InternalSuffix: ecrTestSuffix,
		ECRTokenIssuer: iss, ECRTokenVerifier: verify, DisableLogging: true,
	}
	w := httptest.NewRecorder()
	req := setupECRRequest("AmazonEC2ContainerRegistry_V20150921.GetAuthorizationToken", "{}")
	require.NoError(t, gw.ECR_Request(w, req))
	assert.Equal(t, http.StatusOK, w.Code)
}
