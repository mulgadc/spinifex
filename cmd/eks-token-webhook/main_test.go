package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// postReview drives authenticator.handle with a TokenReview carrying token and
// audiences, returning the decoded response status and HTTP code.
func postReview(t *testing.T, a *authenticator, token string, audiences []string) (tokenReviewStatus, int) {
	t.Helper()
	body, err := json.Marshal(tokenReview{
		APIVersion: "authentication.k8s.io/v1",
		Kind:       "TokenReview",
		Spec:       tokenReviewSpec{Token: token, Audiences: audiences},
	})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/authenticate", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	a.handle(w, req)

	if w.Code != http.StatusOK {
		return tokenReviewStatus{}, w.Code
	}
	var resp tokenReview
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return resp.Status, w.Code
}

func TestHandle_GrantsAndStampsAudiences(t *testing.T) {
	a := &authenticator{
		accountID:   "111122223333",
		clusterName: "alpha",
		review: func(string) (handlers_eks.WebhookTokenReviewResult, error) {
			return handlers_eks.WebhookTokenReviewResult{
				Authenticated: true,
				Username:      "arn:aws:iam::111122223333:role/admin",
				UID:           "AROAEXAMPLE:session",
				Groups:        []string{"system:masters"},
			}, nil
		},
	}

	status, code := postReview(t, a, "tok", []string{"https://kubernetes.default.svc"})
	require.Equal(t, http.StatusOK, code)
	require.True(t, status.Authenticated)
	assert.Equal(t, "arn:aws:iam::111122223333:role/admin", status.User.Username)
	assert.Equal(t, "AROAEXAMPLE:session", status.User.UID)
	assert.Equal(t, []string{"system:masters"}, status.User.Groups)
	assert.Equal(t, []string{"https://kubernetes.default.svc"}, status.Audiences)
}

func TestHandle_DeniesWithoutStampingAudiences(t *testing.T) {
	a := &authenticator{
		clusterName: "alpha",
		review: func(string) (handlers_eks.WebhookTokenReviewResult, error) {
			return handlers_eks.WebhookTokenReviewResult{Authenticated: false}, nil
		},
	}

	status, code := postReview(t, a, "tok", []string{"https://kubernetes.default.svc"})
	require.Equal(t, http.StatusOK, code)
	assert.False(t, status.Authenticated)
	assert.Empty(t, status.User.Username)
	assert.Empty(t, status.Audiences, "unauthenticated review must not stamp audiences")
}

// A broker fault is a 5xx (retryable), not a silent deny — a transient gateway
// outage must not look identical to a rejected token.
func TestHandle_BrokerFaultReturns5xx(t *testing.T) {
	a := &authenticator{
		clusterName: "alpha",
		review: func(string) (handlers_eks.WebhookTokenReviewResult, error) {
			return handlers_eks.WebhookTokenReviewResult{}, errors.New("gateway returned 503")
		},
	}

	_, code := postReview(t, a, "tok", nil)
	assert.Equal(t, http.StatusServiceUnavailable, code)
}

func TestEnsureServingCert_PersistsAndReuses(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "wh.crt")
	keyPath := filepath.Join(dir, "wh.key")

	c1, pem1, err := ensureServingCert(certPath, keyPath)
	require.NoError(t, err)
	require.NotEmpty(t, pem1)
	require.NotNil(t, c1.serverTLSConfig())

	// Second call reuses the persisted pair (same cert bytes).
	_, pem2, err := ensureServingCert(certPath, keyPath)
	require.NoError(t, err)
	assert.Equal(t, pem1, pem2)
}

func TestWriteAPIServerKubeconfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wh.kubeconfig")
	certPEM := []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n")

	require.NoError(t, writeAPIServerKubeconfig(path, "127.0.0.1:8443", certPEM))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	body := string(data)
	assert.Contains(t, body, "server: https://127.0.0.1:8443/authenticate")
	assert.Contains(t, body, base64.StdEncoding.EncodeToString(certPEM))
	assert.Contains(t, body, "current-context: webhook")
}

// The CP VM launches on an instance profile, so cloud-init omits the static
// EKS_ACCESS_KEY/EKS_SECRET_KEY and eksgw.New signs with IMDS creds. loadConfig
// must accept absent static keys; gating on them aborts every IMDS-mode boot,
// leaving the apiserver webhook kubeconfig unwritten.
func TestLoadConfig_AcceptsAbsentStaticCreds(t *testing.T) {
	setEnv := func(t *testing.T, kv map[string]string) {
		t.Helper()
		for _, k := range []string{
			"EKS_GATEWAY_URL", "EKS_ACCESS_KEY", "EKS_SECRET_KEY",
			"EKS_ACCOUNT_ID", "EKS_CLUSTER_NAME",
		} {
			t.Setenv(k, "")
		}
		for k, v := range kv {
			t.Setenv(k, v)
		}
	}

	// IMDS mode: required vars set, static creds absent → accepted.
	setEnv(t, map[string]string{
		"EKS_GATEWAY_URL":  "https://gw:9999",
		"EKS_ACCOUNT_ID":   "000000000001",
		"EKS_CLUSTER_NAME": "toc",
	})
	cfg, err := loadConfig()
	require.NoError(t, err)
	assert.Empty(t, cfg.accessKey)
	assert.Empty(t, cfg.secretKey)

	// Genuinely-required vars are still enforced.
	setEnv(t, map[string]string{
		"EKS_ACCOUNT_ID":   "000000000001",
		"EKS_CLUSTER_NAME": "toc",
	})
	_, err = loadConfig()
	require.Error(t, err)
}
