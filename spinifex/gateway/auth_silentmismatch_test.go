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
	require.Equal(t, http.StatusForbidden, resp.StatusCode)

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
