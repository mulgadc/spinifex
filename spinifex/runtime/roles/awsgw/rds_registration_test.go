package awsgw_test

import (
	"github.com/mulgadc/spinifex/spinifex/gateway"

	rdsawsapi "github.com/mulgadc/spinifex/spinifex/domains/rds/awsapi"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

// withRDS registers RDS on gw with the NATS connection and node count it holds,
// as the awsgw role composes it. Set both on gw before calling.
func withRDS(gw *gateway.GatewayConfig) *gateway.GatewayConfig {
	deps := rdsawsapi.Deps{NATS: gw.NATSConn, ExpectedNodes: gw.ExpectedNodes}
	if quotaFor(gw) != nil {
		deps.Quota = quotaFor(gw)
	}
	b := dispatch.NewBuilder()
	if err := b.Register(rdsawsapi.NewRegistration(deps)); err != nil {
		panic(err)
	}
	gw.Services = b.Build()
	return gw
}
