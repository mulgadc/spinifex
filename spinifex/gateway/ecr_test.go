package gateway

import (
	"encoding/json"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/reqctx"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupECRRequest builds an ECR control-plane request context. It carries a
// full SigV4-shaped identity (not just accountID) so handlers reached through
// it — including GetAuthorizationToken, which now builds a canonical caller
// ARN from the context rather than a best-effort helper — behave exactly as
// they would for a real authenticated user.
func setupECRRequest(target, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	if target != "" {
		req.Header.Set("X-Amz-Target", target)
	}
	ctx := reqctx.WithService(req.Context(), "ecr")
	ctx = withPrincipalField(ctx, fieldAccountID, "123456789012")
	ctx = withPrincipalField(ctx, fieldIdentity, "dev")
	ctx = withPrincipalField(ctx, fieldPrincipalType, gwfix.PrincipalTypeUser)
	ctx = withPrincipalField(ctx, fieldAccessKey, "AKIAECRTESTCONTROLPL1")
	return req.WithContext(ctx)
}

func TestOCIRegistry_StubReturns501(t *testing.T) {
	gw := withVerifier(&GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()})
	r := chi.NewRouter()
	gw.mountRegistry(r)

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
