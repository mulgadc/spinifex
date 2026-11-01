package gateway

import (
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/accountadmission"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/clusterready"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/credverify"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/policyauth"
)

// withRealAuth wires the production verifier, policy evaluator and account
// admission. Only tests that assert a verdict which depends on gateway
// internals (the middleware stash, the admin method gate, the account gate)
// use it; every other gateway test runs on the fake verifier.
func withRealAuth(gw *GatewayConfig) *GatewayConfig {
	withTestAdmin(gw)
	gw.Credentials = credverify.New(gwfix.DepsOf(gw).IAM, gwfix.DepsOf(gw).STS)
	gw.Policy = policyauth.New(gwfix.DepsOf(gw).IAM, policyauth.WithRetryBase(policyRetryBase))
	gw.Admission = accountadmission.New(gwfix.DepsOf(gw).IAM)
	if gwfix.DepsOf(gw).NATS != nil {
		gw.Readiness = realReadiness{gw}
	}
	return gw
}

// withEC2RealAuth is withEC2Verifier on the production capabilities.
func withEC2RealAuth(gw *GatewayConfig) *GatewayConfig {
	return withEC2(withRealAuth(gw))
}

// realReadiness reads the bus connection when asked, so a test that drops it
// after wiring sees the cluster as unavailable.
type realReadiness struct{ gw *GatewayConfig }

func (r realReadiness) Available() bool {
	return gwfix.DepsOf(r.gw).NATS != nil && clusterready.New(gwfix.DepsOf(r.gw).NATS).Available()
}
