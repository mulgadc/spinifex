package gateway

import (
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/reqctx"
	"net/http"
	"net/http/httptest"
	"strings"
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
