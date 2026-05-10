package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

func TestCheckIP_AllowsUnknownIP(t *testing.T) {
	rl := NewAuthRateLimiter()
	defer rl.Stop()

	if errCode := rl.CheckIP("10.0.0.1"); errCode != "" {
		t.Fatalf("expected empty error for unknown IP, got %q", errCode)
	}
}

func TestRecordFailure_BelowThreshold(t *testing.T) {
	rl := NewAuthRateLimiter()
	defer rl.Stop()

	ip := "10.0.0.2"
	for range maxFailures - 1 {
		rl.RecordFailure(ip)
	}

	if errCode := rl.CheckIP(ip); errCode != "" {
		t.Fatalf("expected IP to be allowed after %d failures, got %q", maxFailures-1, errCode)
	}
}

func TestRecordFailure_AtThreshold(t *testing.T) {
	rl := NewAuthRateLimiter()
	defer rl.Stop()

	ip := "10.0.0.3"
	for range maxFailures {
		rl.RecordFailure(ip)
	}

	if errCode := rl.CheckIP(ip); errCode != awserrors.ErrorRequestLimitExceeded {
		t.Fatalf("expected %s after %d failures, got %q", awserrors.ErrorRequestLimitExceeded, maxFailures, errCode)
	}
}

func TestCheckIP_RejectsLockedIP(t *testing.T) {
	rl := NewAuthRateLimiter()
	defer rl.Stop()

	ip := "10.0.0.4"
	for range maxFailures {
		rl.RecordFailure(ip)
	}

	// Should be locked.
	if errCode := rl.CheckIP(ip); errCode != awserrors.ErrorRequestLimitExceeded {
		t.Fatalf("expected locked IP to be rejected, got %q", errCode)
	}

	// Repeated checks should also be rejected.
	if errCode := rl.CheckIP(ip); errCode != awserrors.ErrorRequestLimitExceeded {
		t.Fatalf("expected locked IP to still be rejected, got %q", errCode)
	}
}

func TestRecordSuccess_ClearsState(t *testing.T) {
	rl := NewAuthRateLimiter()
	defer rl.Stop()

	ip := "10.0.0.5"
	// Accumulate failures but stay below threshold.
	for range maxFailures - 1 {
		rl.RecordFailure(ip)
	}

	rl.RecordSuccess(ip)

	// After success, all state should be cleared — can accumulate failures again from 0.
	for range maxFailures - 1 {
		rl.RecordFailure(ip)
	}

	if errCode := rl.CheckIP(ip); errCode != "" {
		t.Fatalf("expected IP to be allowed after success reset, got %q", errCode)
	}
}

func TestRecordSuccess_ClearsLockout(t *testing.T) {
	rl := NewAuthRateLimiter()
	defer rl.Stop()

	ip := "10.0.0.6"
	for range maxFailures {
		rl.RecordFailure(ip)
	}

	// Should be locked.
	if errCode := rl.CheckIP(ip); errCode == "" {
		t.Fatal("expected IP to be locked")
	}

	rl.RecordSuccess(ip)

	// After success, should be unlocked.
	if errCode := rl.CheckIP(ip); errCode != "" {
		t.Fatalf("expected IP to be allowed after success, got %q", errCode)
	}
}

func TestEscalatingBackoff(t *testing.T) {
	rl := NewAuthRateLimiter()
	defer rl.Stop()

	ip := "10.0.0.7"

	// First lockout: 30s
	for range maxFailures {
		rl.RecordFailure(ip)
	}

	rl.mu.Lock()
	rec := rl.records[ip]
	firstLockout := time.Until(rec.lockedUntil)
	rl.mu.Unlock()

	if firstLockout > initialLockout+time.Second || firstLockout < initialLockout-time.Second {
		t.Fatalf("expected first lockout ~%v, got %v", initialLockout, firstLockout)
	}

	// Simulate lockout expiry and trigger second lockout.
	rl.mu.Lock()
	rec.lockedUntil = time.Now().Add(-time.Second) // expired
	rec.failures = nil                             // reset failures for next round
	rl.mu.Unlock()

	for range maxFailures {
		rl.RecordFailure(ip)
	}

	rl.mu.Lock()
	secondLockout := time.Until(rec.lockedUntil)
	rl.mu.Unlock()

	expectedSecond := initialLockout * backoffMultiplier
	if secondLockout > expectedSecond+time.Second || secondLockout < expectedSecond-time.Second {
		t.Fatalf("expected second lockout ~%v, got %v", expectedSecond, secondLockout)
	}

	// Third lockout: 120s
	rl.mu.Lock()
	rec.lockedUntil = time.Now().Add(-time.Second)
	rec.failures = nil
	rl.mu.Unlock()

	for range maxFailures {
		rl.RecordFailure(ip)
	}

	rl.mu.Lock()
	thirdLockout := time.Until(rec.lockedUntil)
	rl.mu.Unlock()

	expectedThird := initialLockout * backoffMultiplier * backoffMultiplier
	if thirdLockout > expectedThird+time.Second || thirdLockout < expectedThird-time.Second {
		t.Fatalf("expected third lockout ~%v, got %v", expectedThird, thirdLockout)
	}

	// Fourth lockout: 30s * 2^3 = 240s = 4m.
	rl.mu.Lock()
	rec.lockedUntil = time.Now().Add(-time.Second)
	rec.failures = nil
	rl.mu.Unlock()

	for range maxFailures {
		rl.RecordFailure(ip)
	}

	rl.mu.Lock()
	fourthLockout := time.Until(rec.lockedUntil)
	rl.mu.Unlock()

	expectedFourth := initialLockout * backoffMultiplier * backoffMultiplier * backoffMultiplier
	if fourthLockout > expectedFourth+time.Second || fourthLockout < expectedFourth-time.Second {
		t.Fatalf("expected fourth lockout ~%v, got %v", expectedFourth, fourthLockout)
	}

	// Fifth lockout: 30s * 2^4 = 480s, capped at maxLockout (300s).
	rl.mu.Lock()
	rec.lockedUntil = time.Now().Add(-time.Second)
	rec.failures = nil
	rl.mu.Unlock()

	for range maxFailures {
		rl.RecordFailure(ip)
	}

	rl.mu.Lock()
	fifthLockout := time.Until(rec.lockedUntil)
	rl.mu.Unlock()

	if fifthLockout > maxLockout+time.Second || fifthLockout < maxLockout-time.Second {
		t.Fatalf("expected fifth lockout to cap at ~%v, got %v", maxLockout, fifthLockout)
	}
}

func TestFailureWindowSliding(t *testing.T) {
	rl := NewAuthRateLimiter()
	defer rl.Stop()

	ip := "10.0.0.8"

	// Inject failures that are outside the sliding window.
	rl.mu.Lock()
	rec := &ipRecord{}
	oldTime := time.Now().Add(-failureWindow - time.Second)
	for range maxFailures - 1 {
		rec.failures = append(rec.failures, oldTime)
	}
	rl.records[ip] = rec
	rl.mu.Unlock()

	// Add one recent failure — total "recent" failures should be just 1.
	rl.RecordFailure(ip)

	if errCode := rl.CheckIP(ip); errCode != "" {
		t.Fatalf("expected IP to be allowed (old failures expired), got %q", errCode)
	}
}

func TestCleanup_EvictsStaleEntries(t *testing.T) {
	rl := NewAuthRateLimiter()
	defer rl.Stop()

	ip := "10.0.0.9"

	// Insert a stale entry: lockout expired and all failures old.
	rl.mu.Lock()
	rl.records[ip] = &ipRecord{
		failures:    []time.Time{time.Now().Add(-failureWindow - time.Second)},
		lockedUntil: time.Now().Add(-time.Second),
		lockouts:    1,
	}
	rl.mu.Unlock()

	rl.cleanup()

	rl.mu.Lock()
	_, exists := rl.records[ip]
	rl.mu.Unlock()

	if exists {
		t.Fatal("expected stale entry to be evicted by cleanup")
	}
}

func TestCleanup_KeepsActiveEntries(t *testing.T) {
	rl := NewAuthRateLimiter()
	defer rl.Stop()

	ip := "10.0.0.10"

	// Insert an entry that's still locked.
	rl.mu.Lock()
	rl.records[ip] = &ipRecord{
		failures:    []time.Time{time.Now()},
		lockedUntil: time.Now().Add(30 * time.Second),
		lockouts:    1,
	}
	rl.mu.Unlock()

	rl.cleanup()

	rl.mu.Lock()
	_, exists := rl.records[ip]
	rl.mu.Unlock()

	if !exists {
		t.Fatal("expected active entry to be kept by cleanup")
	}
}

func TestConcurrentAccess(t *testing.T) {
	rl := NewAuthRateLimiter()
	defer rl.Stop()

	var wg sync.WaitGroup
	ips := []string{"10.0.0.20", "10.0.0.21", "10.0.0.22"}

	for _, ip := range ips {
		for range 20 {
			wg.Go(func() {
				rl.CheckIP(ip)
				rl.RecordFailure(ip)
				rl.RecordSuccess(ip)
				rl.CheckIP(ip)
			})
		}
	}

	wg.Wait()
}

func TestExtractClientIP(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		expected   string
	}{
		{"IPv4 with port", "192.168.1.1:12345", "192.168.1.1"},
		{"IPv6 with port", "[::1]:12345", "::1"},
		{"IPv4 bare", "192.168.1.1", "192.168.1.1"},
		{"IPv6 full with port", "[2001:db8::1]:443", "2001:db8::1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &http.Request{RemoteAddr: tt.remoteAddr}
			got := extractClientIP(r)
			if got != tt.expected {
				t.Errorf("extractClientIP(%q) = %q, want %q", tt.remoteAddr, got, tt.expected)
			}
		})
	}
}

// setupTestAppWithRateLimiter creates a test HTTP handler with SigV4 auth and
// the given rate limiter attached. A real NATS test connection is attached so
// the SigV4 middleware's cluster-unavailable short-circuit (mulga-siv-23) does
// not fire and mask rate-limit behaviour.
func setupTestAppWithRateLimiter(t *testing.T, accessKey, secretKey string, rl *AuthRateLimiter) http.Handler {
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
	rl := NewAuthRateLimiter()
	defer rl.Stop()

	handler := setupTestAppWithRateLimiter(t, testAccessKey, testSecretKey, rl)

	// Send maxFailures requests with invalid signatures to trigger lockout.
	for range maxFailures {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = "localhost:9999"
		req.RemoteAddr = "10.99.0.1:54321"
		req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAINVALIDKEY000000/20240101/us-east-1/ec2/aws4_request, SignedHeaders=host;x-amz-date, Signature=badsig")
		req.Header.Set("X-Amz-Date", time.Now().UTC().Format("20060102T150405Z"))
		doRequest(handler, req)
	}

	// Next request from same IP should be rate-limited.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	req.RemoteAddr = "10.99.0.1:54321"
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAINVALIDKEY000000/20240101/us-east-1/ec2/aws4_request, SignedHeaders=host;x-amz-date, Signature=badsig")
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
	rl := NewAuthRateLimiter()
	defer rl.Stop()

	handler := setupTestAppWithRateLimiter(t, testAccessKey, testSecretKey, rl)

	// Accumulate failures below threshold.
	for range maxFailures - 1 {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = "localhost:9999"
		req.RemoteAddr = "10.99.0.2:54321"
		req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAINVALIDKEY000000/20240101/us-east-1/ec2/aws4_request, SignedHeaders=host;x-amz-date, Signature=badsig")
		req.Header.Set("X-Amz-Date", time.Now().UTC().Format("20060102T150405Z"))
		doRequest(handler, req)
	}

	// Now send a valid request — should succeed and clear failure state.
	authHeader, timestamp := generateTestAuthHeader("GET", "/", "", "", testAccessKey, testSecretKey, testRegion, testService)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	req.RemoteAddr = "10.99.0.2:54321"
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("X-Amz-Date", timestamp)

	resp := doRequest(handler, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on valid request, got %d", resp.StatusCode)
	}

	// Verify state was cleared — should be no record.
	rl.mu.Lock()
	_, exists := rl.records["10.99.0.2"]
	rl.mu.Unlock()

	if exists {
		t.Fatal("expected IP record to be cleared after successful auth")
	}
}
