package gateway

import (
	authlimit "github.com/mulgadc/spinifex/spinifex/ingress/aws/ratelimit"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"strconv"
)

// setupTestAppWithRateLimiter creates a test HTTP handler with SigV4 auth and
// the given rate limiter attached. A real NATS connection is used so the
// cluster-unavailable short-circuit does not mask rate-limit behaviour.
func setupTestAppWithRateLimiter(t *testing.T, accessKey, secretKey string, rl *authlimit.AuthRateLimiter) http.Handler {
	t.Helper()

	encryptedSecret, err := handlers_iam.EncryptSecret(secretKey, testMasterKey)
	if err != nil {
		panic("failed to encrypt test secret: " + err.Error())
	}

	mockSvc := &mockIAMService{
		masterKey: testMasterKey,
		accessKeys: map[string]*handlers_iam.AccessKey{
			accessKey: {
				AccessKeyID:     accessKey,
				SecretAccessKey: encryptedSecret,
				UserName:        "root",
				Status:          "Active",
			},
		},
	}

	ns, _ := testutil.StartTestNATS(t)
	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	gw := &GatewayConfig{
		DisableLogging: true,
		Region:         testRegion,
		IAMService:     mockSvc,
		RateLimiter:    rl,
		NATSConn:       nc,
	}

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	})

	return r
}

func TestRateLimitIntegration_LockedIPGets503(t *testing.T) {
	rl := authlimit.NewAuthRateLimiter()
	defer rl.Stop()

	handler := setupTestAppWithRateLimiter(t, testAccessKey, testSecretKey, rl)

	// Send maxFailures requests with invalid signatures to trigger lockout.
	for range authlimit.MaxFailures {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = "localhost:9999"
		req.RemoteAddr = "10.99.0.1:54321"
		req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAINVALIDKEY000000/20240101/us-east-1/ec2/aws4_request, SignedHeaders=host;x-amz-date, Signature=0000000000000000000000000000000000000000000000000000000000000000")
		req.Header.Set("X-Amz-Date", time.Now().UTC().Format("20060102T150405Z"))
		doRequest(handler, req)
	}

	// Next request from same IP should be rate-limited.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	req.RemoteAddr = "10.99.0.1:54321"
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAINVALIDKEY000000/20240101/us-east-1/ec2/aws4_request, SignedHeaders=host;x-amz-date, Signature=0000000000000000000000000000000000000000000000000000000000000000")
	req.Header.Set("X-Amz-Date", time.Now().UTC().Format("20060102T150405Z"))

	resp := doRequest(handler, req)
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503 for rate-limited IP, got %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "RequestLimitExceeded") {
		t.Errorf("expected RequestLimitExceeded in response, got: %s", string(body))
	}
}

func TestRateLimitIntegration_SuccessResetsLockout(t *testing.T) {
	rl := authlimit.NewAuthRateLimiter()
	defer rl.Stop()

	handler := setupTestAppWithRateLimiter(t, testAccessKey, testSecretKey, rl)

	// Accumulate failures below threshold.
	for range authlimit.MaxFailures - 1 {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = "localhost:9999"
		req.RemoteAddr = "10.99.0.2:54321"
		req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAINVALIDKEY000000/20240101/us-east-1/ec2/aws4_request, SignedHeaders=host;x-amz-date, Signature=0000000000000000000000000000000000000000000000000000000000000000")
		req.Header.Set("X-Amz-Date", time.Now().UTC().Format("20060102T150405Z"))
		doRequest(handler, req)
	}

	// Now send a valid request — should succeed and clear failure state.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	req.RemoteAddr = "10.99.0.2:54321"
	signTestRequest(t, req, nil, testAccessKey, testSecretKey)

	resp := doRequest(handler, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on valid request, got %d", resp.StatusCode)
	}

	// Cleared state reads at the boundary: MaxFailures-1 fresh failures leave
	// the address open, which any surviving failure would push over.
	const ip = "10.99.0.2"
	recordProbeFailures(rl, ip, "after-success", authlimit.MaxFailures-1)
	assert.Empty(t, rl.CheckIP(ip), "success must clear the earlier failures")
	recordProbeFailures(rl, ip, "edge", 1)
	assert.Equal(t, awserrors.ErrorRequestLimitExceeded, rl.CheckIP(ip),
		"the probes must count, or the check above proves nothing")
}

// recordProbeFailures records n failures distinct from each other and from any
// fingerprint the gateway produces, so each one counts toward the lockout.
func recordProbeFailures(rl *authlimit.AuthRateLimiter, ip, batch string, n int) {
	for i := range n {
		rl.RecordFailure(ip, authlimit.Fingerprint("test-probe", batch, strconv.Itoa(i)))
	}
}

// End to end: a client whose credential will never resolve keeps getting the
// verdict that says so, and never the 503 that tells it to retry.
func TestStaleCredentialNeverLocksTheAddressOut(t *testing.T) {
	handler, _ := auditRouter(t, map[string]*handlers_iam.AccessKey{})

	for range authlimit.MaxFailures * 3 {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, signedRequest("10.15.8.14:54321"))
		require.Equal(t, http.StatusForbidden, w.Code, "a dead credential is a client fault, not throttling")
	}
}
