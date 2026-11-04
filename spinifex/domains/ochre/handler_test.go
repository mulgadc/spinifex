package ochre

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/envelope"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
			assert.Equal(t, envelope.JSONContentType, w.Header().Get("Content-Type"))

			var env struct {
				Type    string `json:"__type"`
				Message string `json:"message"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
			assert.Equal(t, tc.wantType, env.Type)
			assert.Equal(t, "boom", env.Message)
		})
	}
}
