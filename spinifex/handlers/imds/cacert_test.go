package handlers_imds

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testCAPEM = "-----BEGIN CERTIFICATE-----\ntestca\n-----END CERTIFICATE-----\n"

// newCATestService builds a service whose CA cache points at a temp file holding
// pem, returning the handler and the file path so a test can rotate it.
func newCATestService(t *testing.T, pem string) (http.Handler, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	if pem != "" {
		require.NoError(t, os.WriteFile(path, []byte(pem), 0o600))
	}
	// A resolvable instance, because the meta-data listing now 500s without one.
	svc, _ := newTestService(&fakeResolver{eni: testENI(), inst: &instanceFacts{}}, &fakeIAM{}, &fakeAssumer{})
	svc.caCert = newCACertCache(path)
	return withTapENI(svc.httpHandler(), testENI()), path
}

// TestCACert_ServedWithoutToken is the load-bearing assertion for the route:
// the CA is public material, so a guest fetches it with a plain GET and no
// IMDSv2 token handshake. Every other metadata path 401s under those conditions.
func TestCACert_ServedWithoutToken(t *testing.T) {
	h, _ := newCATestService(t, testCAPEM)

	rec := get(t, h, pathSpinifexCACert, "")

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, testCAPEM, rec.Body.String())
	assert.Equal(t, caCertContentType, rec.Header().Get("Content-Type"))
}

// TestCACert_MissingFileIs404 pins the fail-soft behaviour: a deployment with no
// CA on disk serves a 404, never a 500, and vpcd still starts.
func TestCACert_MissingFileIs404(t *testing.T) {
	h, _ := newCATestService(t, "")

	rec := get(t, h, pathSpinifexCACert, "")

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestCACert_RotationServedWithoutRestart covers the reason the cache keys on
// modtime+size: rotating the CA on disk must reach guests without restarting vpcd.
func TestCACert_RotationServedWithoutRestart(t *testing.T) {
	h, path := newCATestService(t, testCAPEM)

	require.Equal(t, http.StatusOK, get(t, h, pathSpinifexCACert, "").Code)

	rotated := "-----BEGIN CERTIFICATE-----\nrotated\n-----END CERTIFICATE-----\n"
	require.NoError(t, os.WriteFile(path, []byte(rotated), 0o600))
	require.NoError(t, os.Chtimes(path, time.Now().Add(time.Second), time.Now().Add(time.Second)))

	rec := get(t, h, pathSpinifexCACert, "")

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, rotated, rec.Body.String())
}

// TestCACert_ForwardedRejected confirms the token-free route did not opt out of
// the SSRF guard: rejectForwarded wraps the whole mux, this path included.
func TestCACert_ForwardedRejected(t *testing.T) {
	h, _ := newCATestService(t, testCAPEM)

	req := httptest.NewRequest(http.MethodGet, "http://"+MetaDataServerIP+pathSpinifexCACert, nil)
	req.RemoteAddr = testIP + ":50000"
	req.Header.Set(hdrForwardedFor, "203.0.113.99")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Empty(t, rec.Body.String())
}

// TestCACert_RejectsNonGET keeps the route read-only.
func TestCACert_RejectsNonGET(t *testing.T) {
	h, _ := newCATestService(t, testCAPEM)

	req := httptest.NewRequest(http.MethodPost, "http://"+MetaDataServerIP+pathSpinifexCACert, nil)
	req.RemoteAddr = testIP + ":50000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Equal(t, http.MethodGet, rec.Header().Get("Allow"))
}

// TestCACert_AbsentFromAWSMetadataListing pins the placement decision: the route
// is a Spinifex extension outside /latest, so the AWS-compatible surface a guest
// crawls is byte-identical to EC2's and cloud-init never descends into it.
func TestCACert_AbsentFromAWSMetadataListing(t *testing.T) {
	h, _ := newCATestService(t, testCAPEM)
	token := issueToken(t, h)

	rec := get(t, h, pathMetaDataRoot, token)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "spinifex")
	assert.NotContains(t, rec.Body.String(), "ca.pem")
}
