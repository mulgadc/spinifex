package gateway_bedrock

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteJSONResponse_WritesBodyAndContentType(t *testing.T) {
	type payload struct {
		Foo string `json:"foo"`
	}

	w := httptest.NewRecorder()
	WriteJSONResponse(w, payload{Foo: "bar"})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, JSONContentType, w.Header().Get("Content-Type"))

	var out payload
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, "bar", out.Foo)
}

func TestWriteJSONError_WritesEnvelopeAndStatus(t *testing.T) {
	cases := []struct {
		name       string
		code       string
		httpStatus int
		wantStatus int
		wantType   string
	}{
		{"explicit status", awserrors.ErrorValidationException, http.StatusBadRequest, http.StatusBadRequest, "ValidationException"},
		{"zero status defaults to 500", awserrors.ErrorInternalError, 0, http.StatusInternalServerError, "InternalErrorException"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			WriteJSONError(w, tc.code, "boom", tc.httpStatus)

			assert.Equal(t, tc.wantStatus, w.Code)
			assert.Equal(t, JSONContentType, w.Header().Get("Content-Type"))

			var env jsonError
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
			assert.Equal(t, tc.wantType, env.Type)
			assert.Equal(t, "boom", env.Message)
		})
	}
}

func TestGenerateJSONError_AppendsExceptionSuffixIdempotently(t *testing.T) {
	cases := []struct {
		name     string
		code     string
		wantType string
	}{
		{"bare code gets suffix", "Validation", "ValidationException"},
		{"already-suffixed code unchanged", "ValidationException", "ValidationException"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := generateJSONError(tc.code, "msg")
			var env jsonError
			require.NoError(t, json.Unmarshal(body, &env))
			assert.Equal(t, tc.wantType, env.Type)
			assert.Equal(t, "msg", env.Message)
		})
	}
}
