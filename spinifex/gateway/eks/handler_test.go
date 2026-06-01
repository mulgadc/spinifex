package gateway_eks

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateEKSErrorResponse_ShapesExceptionSuffix(t *testing.T) {
	body := GenerateEKSErrorResponse("ResourceNotFound", "Cluster does not exist")
	var env EKSJSONError
	require.NoError(t, json.Unmarshal(body, &env))
	assert.Equal(t, "ResourceNotFoundException", env.Type)
	assert.Equal(t, "Cluster does not exist", env.Message)
}

func TestWriteJSONError_SetsContentTypeAndStatus(t *testing.T) {
	w := httptest.NewRecorder()
	WriteJSONError(w, "NotImplemented", "Operation not implemented", http.StatusNotImplemented)
	assert.Equal(t, http.StatusNotImplemented, w.Code)
	assert.Equal(t, JSONContentType, w.Header().Get("Content-Type"))

	var env EKSJSONError
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.Equal(t, "NotImplementedException", env.Type)
	assert.Equal(t, "Operation not implemented", env.Message)
}

func TestWriteJSONResponse_SerializesObject(t *testing.T) {
	w := httptest.NewRecorder()
	WriteJSONResponse(w, map[string]string{"foo": "bar"})
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, JSONContentType, w.Header().Get("Content-Type"))

	var got map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, "bar", got["foo"])
}

type sampleInput struct {
	Name string `json:"name"`
}

func TestParseJSONBody_EmptyBodyOK(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/clusters", nil)
	got, err := ParseJSONBody[sampleInput](req)
	require.NoError(t, err)
	assert.Empty(t, got.Name)
}

func TestParseJSONBody_DecodesBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/clusters", bytes.NewReader([]byte(`{"name":"alpha"}`)))
	got, err := ParseJSONBody[sampleInput](req)
	require.NoError(t, err)
	assert.Equal(t, "alpha", got.Name)
}

func TestParseJSONBody_InvalidJSONErrors(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/clusters", bytes.NewReader([]byte(`{not-json}`)))
	_, err := ParseJSONBody[sampleInput](req)
	require.Error(t, err)
}
