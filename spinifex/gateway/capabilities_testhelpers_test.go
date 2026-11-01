package gateway

import (
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	operatoradmin "github.com/mulgadc/spinifex/spinifex/operator/admin"
	"net/http"
	"time"

	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/credential"
)

// policyRetryBase shortens the backoff between policy-resolve attempts.
var policyRetryBase = 200 * time.Millisecond

// allowPolicy permits every request; tests that assert an authorization
// verdict run against the real evaluator in the awsgw packages instead.
type allowPolicy struct{}

func (allowPolicy) RequestConditionKeys(*http.Request, credential.Principal) iampolicy.ConditionKeys {
	return iampolicy.ConditionKeys{}
}

func (allowPolicy) Evaluate(credential.Principal, string, []string, iampolicy.ConditionKeys) error {
	return nil
}

type allowAdmission struct{}

func (allowAdmission) Admit(string, string) error { return nil }

type natsReadiness struct{ gw *GatewayConfig }

func (n natsReadiness) Available() bool {
	return gwfix.DepsOf(n.gw).NATS != nil && gwfix.DepsOf(n.gw).NATS.IsConnected()
}

// withFakeCapabilities fills whichever capability a test left unset.
func withFakeCapabilities(gw *GatewayConfig) *GatewayConfig {
	withTestAdmin(gw)
	if gw.Policy == nil {
		gw.Policy = allowPolicy{}
	}
	if gw.Admission == nil {
		gw.Admission = allowAdmission{}
	}
	if gw.Readiness == nil && gwfix.DepsOf(gw).NATS != nil {
		gw.Readiness = natsReadiness{gw}
	}
	return gw
}

// withTestAdmin points the /admin surface at the spinifex registration, as the
// composing role does.
func withTestAdmin(gw *GatewayConfig) {
	if gw.Admin.Service == "" {
		gw.Admin = AdminSurface{Service: operatoradmin.ServiceName, WithRequestID: operatoradmin.WithRequestID}
	}
}
