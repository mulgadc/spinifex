package awsgw_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOCIRegistry_StubReturns501(t *testing.T) {
	r := chi.NewRouter()
	awsgw.PublicRoutes(nil, nil)(r)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v2/team/app/tags/list", nil))

	assert.Equal(t, http.StatusNotImplemented, w.Code)

	var body struct {
		Errors []struct {
			Code string `json:"code"`
		} `json:"errors"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Errors, 1)
	assert.Equal(t, "UNSUPPORTED", body.Errors[0].Code)
}
