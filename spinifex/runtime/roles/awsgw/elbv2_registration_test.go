package awsgw_test

import (
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/pipeline"

	elbv2awsapi "github.com/mulgadc/spinifex/spinifex/domains/elbv2/awsapi"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

// withELBv2 registers ELBv2 on gw with the NATS connection and quota it holds,
// as the awsgw role composes it. Set both on gw before calling.
func withELBv2(gw *pipeline.GatewayConfig) *pipeline.GatewayConfig {
	b := dispatch.NewBuilder()
	reg := elbv2awsapi.NewRegistration(elbv2awsapi.Deps{NATS: gwfix.DepsOf(gw).NATS, Quota: quotaFor(gw)})
	if err := b.Register(reg); err != nil {
		panic(err)
	}
	gw.Services = b.Build()
	return gw
}

// elbv2ServedActions lists every action the ELBv2 service dispatches.
func elbv2ServedActions() []string {
	return elbv2awsapi.ActionNames()
}
