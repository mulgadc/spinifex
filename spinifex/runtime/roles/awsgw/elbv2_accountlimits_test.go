//test:in-package — drives the ELBv2 service through the gateway test helpers
// and the quota fixtures, none of which are reachable from outside the package.

package awsgw_test

import (
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/reqctx"
	"net/http"
	"net/http/httptest"
	"strings"
)

// elbv2Request builds an authenticated Query request for body, as the SigV4
// middleware would hand it to the service.
func elbv2Request(accountID, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	ctx := reqctx.WithService(req.Context(), "elasticloadbalancing")
	ctx = withPrincipalField(ctx, fieldAccountID, accountID)
	return withTestIdentity(req.WithContext(ctx))
}
