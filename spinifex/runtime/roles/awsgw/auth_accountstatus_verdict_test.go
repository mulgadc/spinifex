package awsgw_test

//test:in-package — the status gate is an unexported step of the auth
// middleware, and its cache is deliberately not part of the package's API.

import (
	"net/http"
	"testing"

	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
)

// Until this gate existed, account status was enforced only in ECR's principal
// resolution, so suspending an account left EC2, S3 and STS wide open.
func TestAuthDeniesASuspendedAccount(t *testing.T) {
	r, req := statusGateRouter(t, statusGateService(t, "123456789012", iamdomain.AccountStatusSuspended))

	if resp := doRequest(r, req); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for a SUSPENDED account, got %d", resp.StatusCode)
	}
}
