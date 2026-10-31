package gateway

import (
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

func (n natsReadiness) Available() bool { return n.gw.NATSConn != nil && n.gw.NATSConn.IsConnected() }

// withFakeCapabilities fills whichever capability a test left unset.
func withFakeCapabilities(gw *GatewayConfig) *GatewayConfig {
	if gw.Policy == nil {
		gw.Policy = allowPolicy{}
	}
	if gw.Admission == nil {
		gw.Admission = allowAdmission{}
	}
	if gw.Readiness == nil && gw.NATSConn != nil {
		gw.Readiness = natsReadiness{gw}
	}
	return gw
}
