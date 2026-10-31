package gateway

import (
	"time"

	"github.com/mulgadc/spinifex/spinifex/ingress/aws/pipeline"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/accountadmission"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/clusterready"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/policyauth"
)

// policyRetryBase shortens the backoff between policy-resolve attempts.
var policyRetryBase = 200 * time.Millisecond

func init() {
	defaultCapabilities = func(gw *GatewayConfig) (pipeline.PolicyEvaluator, pipeline.AccountAdmission, pipeline.Readiness) {
		var readiness pipeline.Readiness
		if gw.NATSConn != nil {
			readiness = clusterready.New(gw.NATSConn)
		}
		return policyauth.New(gw.IAMService, policyauth.WithRetryBase(policyRetryBase)), accountadmission.New(gw.IAMService), readiness
	}
}
