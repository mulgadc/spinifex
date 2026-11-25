package awsgw_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/pipeline"
	"github.com/mulgadc/spinifex/spinifex/operator/consolebranding"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw"
	"github.com/stretchr/testify/assert"
)

// The login page loads the branding before anyone has signed in, so the
// route must answer an unsigned request through the full gateway chain.
func TestConsoleBrandingRoute_ServedAheadOfSigV4(t *testing.T) {
	cache := &consolebranding.Cache{}
	cache.Set(&consolebranding.Record{ProductName: "Acme Cloud"}, 2)
	branding := consolebranding.Handler{Cache: cache}
	gw := withVerifier(gwfix.WithDeps(&pipeline.Config{DisableLogging: true, PublicRoutes: awsgw.PublicRoutes(nil, nil, branding)},
		gwfix.Deps{IAM: allowAllIAMService()}))
	h := gw.SetupRoutes()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/console/branding/branding.json", nil))

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.JSONEq(t, `{"revision":2,"productName":"Acme Cloud"}`, w.Body.String())
}
