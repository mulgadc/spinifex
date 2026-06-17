package gateway_ecr

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func decodeEnvelope(t *testing.T, body []byte) ociErrorEnvelope {
	t.Helper()
	var env ociErrorEnvelope
	require.NoError(t, json.Unmarshal(body, &env))
	return env
}

func TestAPIVersion(t *testing.T) {
	w := httptest.NewRecorder()
	APIVersion(w, httptest.NewRequest(http.MethodGet, "/v2/", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "registry/2.0", w.Header().Get("Docker-Distribution-Api-Version"))
}

func TestNotImplemented(t *testing.T) {
	w := httptest.NewRecorder()
	NotImplemented(w, httptest.NewRequest(http.MethodGet, "/v2/team/app/tags/list", nil))

	assert.Equal(t, http.StatusNotImplemented, w.Code)
	assert.Equal(t, OCIContentType, w.Header().Get("Content-Type"))

	env := decodeEnvelope(t, w.Body.Bytes())
	require.Len(t, env.Errors, 1)
	assert.Equal(t, "UNSUPPORTED", env.Errors[0].Code)
	assert.NotEmpty(t, env.Errors[0].Message)
}

func TestWriteError(t *testing.T) {
	w := httptest.NewRecorder()
	WriteError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest tagged v1 not found")

	assert.Equal(t, http.StatusNotFound, w.Code)
	env := decodeEnvelope(t, w.Body.Bytes())
	require.Len(t, env.Errors, 1)
	assert.Equal(t, "MANIFEST_UNKNOWN", env.Errors[0].Code)
	assert.Equal(t, "manifest tagged v1 not found", env.Errors[0].Message)
}
