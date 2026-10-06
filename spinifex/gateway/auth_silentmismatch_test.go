package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/utils"
)

// captureLogs redirects the default slog logger into a buffer for the duration
// of a test, so an assertion can read what the middleware actually emitted.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// Both of these paths answer SignatureDoesNotMatch, which on the wire is
// identical to a canonicalisation mismatch. Without a log there is no way to
// tell them apart after the fact, so the log line is the contract under test.

func TestSigV4Auth_SkewedRequestIsLogged(t *testing.T) {
	handler := setupTestApp(testAccessKey, testSecretKey)
	logs := captureLogs(t)

	skewed := time.Now().UTC().Add(-30 * time.Minute)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	signTestRequest(t, req, nil, testAccessKey, testSecretKey, skewed)

	resp := doRequest(handler, req)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)

	out := logs.String()
	require.Contains(t, out, "request time too skewed")
	// The two timestamps are the point: they are what identifies skew as the
	// cause rather than a signing defect.
	require.Contains(t, out, "requestTime="+skewed.Format("20060102T150405Z"))
	require.Contains(t, out, "serverTime=")
}

func TestSigV4Auth_UnsupportedServiceIsLogged(t *testing.T) {
	handler := setupTestApp(testAccessKey, testSecretKey)
	logs := captureLogs(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"

	sum := sha256.Sum256(nil)
	payloadHash := hex.EncodeToString(sum[:])
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	require.NoError(t, v4.NewSigner().SignHTTP(context.Background(),
		aws.Credentials{AccessKeyID: testAccessKey, SecretAccessKey: testSecretKey},
		req, payloadHash, "notaservice", testRegion, time.Now().UTC()))

	resp := doRequest(handler, req)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	out := logs.String()
	require.Contains(t, out, "unsupported service in credential scope")
	require.Contains(t, out, "service=notaservice")
}

// The parse-error branch answers IncompleteSignature and was likewise silent,
// so a malformed envelope left no record of which validation stage rejected it.
func TestSigV4Auth_MalformedEnvelopeIsLogged(t *testing.T) {
	handler := setupTestApp(testAccessKey, testSecretKey)
	logs := captureLogs(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=nonsense, Signature=deadbeef")

	resp := doRequest(handler, req)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	out := logs.String()
	require.Contains(t, out, "malformed signature envelope")
	require.Contains(t, out, "sourceIP=")
}

// signingTime must prefer the header, fall back to the presigned query arg, and
// tolerate a request carrying neither rather than panicking on a nil URL query.
func TestSigningTime_Sources(t *testing.T) {
	hdr := httptest.NewRequest(http.MethodGet, "/", nil)
	hdr.Header.Set("X-Amz-Date", "20260729T165540Z")
	require.Equal(t, "20260729T165540Z", signingTime(hdr))

	presigned := httptest.NewRequest(http.MethodGet, "/?X-Amz-Date=20260729T165541Z", nil)
	require.Equal(t, "20260729T165541Z", signingTime(presigned))

	none := httptest.NewRequest(http.MethodGet, "/", nil)
	require.Empty(t, signingTime(none))
}

// A proxied request reaches the gateway from loopback. The auth-failure line
// must name the client the request audit line names, while the lockout stays
// keyed on the connection peer.
func TestSigV4Auth_FailureLogsLoopbackGatedClientIP(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		wantLogIP  string
	}{
		{"proxied via loopback", "127.0.0.1:41234", "203.0.113.7"},
		{"direct client cannot choose the logged IP", "198.51.100.9:41234", "198.51.100.9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rl := NewAuthRateLimiter()
			defer rl.Stop()
			gw := &GatewayConfig{
				DisableLogging: true,
				Region:         testRegion,
				IAMService:     &mockIAMService{masterKey: testMasterKey},
				STSService:     &mockSTSService{},
				RateLimiter:    rl,
			}
			handler := gw.SigV4AuthMiddleware()(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("an unknown session credential must not reach the handler")
			}))
			logs := captureLogs(t)

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Host = "localhost:9999"
			req.RemoteAddr = tc.remoteAddr
			req.Header.Set("X-Real-IP", "203.0.113.7")
			signSessionRequest(t, req, nil, testSessionAKID, testSecretKey, testSessionToken)

			resp := doRequest(handler, req)
			require.Equal(t, http.StatusForbidden, resp.StatusCode)

			out := logs.String()
			require.Contains(t, out, "session credential not found")
			require.Contains(t, out, "sourceIP="+tc.wantLogIP)

			rl.mu.RLock()
			defer rl.mu.RUnlock()
			require.NotNil(t, rl.records[utils.ClientIP(tc.remoteAddr)], "lockout must key on the connection peer")
		})
	}
}
