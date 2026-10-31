package gateway

import "github.com/mulgadc/spinifex/spinifex/ingress/aws/pipeline"

// defaultCapabilities builds the capabilities a GatewayConfig left unset from
// its IAM service and bus connection. Production composition sets all of them and
// leaves this nil.
var defaultCapabilities func(gw *GatewayConfig) (pipeline.PolicyEvaluator, pipeline.AccountAdmission, pipeline.Readiness)

// fillDefaultCapabilities sets whichever capability is nil from
// defaultCapabilities. With no defaults installed it changes nothing.
func (gw *GatewayConfig) fillDefaultCapabilities() {
	if defaultCapabilities == nil || (gw.Policy != nil && gw.Admission != nil && gw.Readiness != nil) {
		return
	}
	policy, admission, readiness := defaultCapabilities(gw)
	if gw.Policy == nil {
		gw.Policy = policy
	}
	if gw.Admission == nil {
		gw.Admission = admission
	}
	if gw.Readiness == nil {
		gw.Readiness = readiness
	}
}
