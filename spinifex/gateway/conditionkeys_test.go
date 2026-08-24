package gateway

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequestConditionKeys_PopulatesAvailableKeys(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.TLS = &tls.ConnectionState{}
	r = r.WithContext(context.WithValue(r.Context(), ctxClientIP, "10.4.1.9"))
	principal := principalContext{identity: "alice", accountID: "000000000001"}

	keys := requestConditionKeys(r, principal)

	assert.Equal(t, iampolicy.ConditionKeys{
		iampolicy.KeySourceIP:         "10.4.1.9",
		iampolicy.KeySecureTransport:  "true",
		iampolicy.KeyUsername:         "alice",
		iampolicy.KeyPrincipalAccount: "000000000001",
	}, keys)
}

// s3:prefix has no meaning on the AWS API path, so a policy conditioned on it
// must not fire here even though the same document works at predastore's door.
func TestRequestConditionKeys_OmitsS3Prefix(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/?prefix=home/", nil)
	keys := requestConditionKeys(r, principalContext{identity: "alice"})

	assert.NotContains(t, keys, iampolicy.KeyS3Prefix)
	assert.Equal(t, "false", keys[iampolicy.KeySecureTransport])
}

// An unknown source address leaves the key absent, not empty: absent is what
// makes a condition on it evaluate false for the right reason.
func TestRequestConditionKeys_OmitsUnknownSourceIP(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	keys := requestConditionKeys(r, principalContext{identity: "alice"})

	assert.NotContains(t, keys, iampolicy.KeySourceIP)
}

// The auth middleware already computes clientIP for rate limiting; it must also
// reach policy evaluation, or every aws:SourceIp condition evaluates false.
func TestSigV4Auth_StoresClientIP(t *testing.T) {
	encryptedSecret, err := handlers_iam.EncryptSecret(testSecretKey, testMasterKey)
	require.NoError(t, err)
	gw := &GatewayConfig{
		DisableLogging: true,
		Region:         testRegion,
		IAMService: &mockIAMService{
			masterKey: testMasterKey,
			accessKeys: map[string]*handlers_iam.AccessKey{
				testAccessKey: {
					AccessKeyID:     testAccessKey,
					SecretAccessKey: encryptedSecret,
					UserName:        "root",
					Status:          "Active",
				},
			},
		},
	}

	var got string
	router := chi.NewRouter()
	router.Use(gw.SigV4AuthMiddleware())
	router.HandleFunc("/*", func(w http.ResponseWriter, r *http.Request) {
		got = mustCtxString(r, ctxClientIP)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	req.RemoteAddr = "10.4.1.9:52344"
	signTestRequest(t, req, nil, testAccessKey, testSecretKey)

	resp := doRequest(router, req)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "10.4.1.9", got)
}
