package awsgw_test

import (
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	acmawsapi "github.com/mulgadc/spinifex/spinifex/domains/acm/awsapi"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/pipeline"
)

// withACM registers the ACM control plane on gw backed by deps, as the awsgw
// role composes it. An unset deps.NATS takes gw.NATSConn.
func withACM(gw *pipeline.GatewayConfig, deps acmawsapi.Deps) *pipeline.GatewayConfig {
	if deps.NATS == nil {
		deps.NATS = gwfix.DepsOf(gw).NATS
	}
	b := dispatch.NewBuilder()
	if err := b.Register(acmawsapi.NewRegistration(deps)); err != nil {
		panic(err)
	}
	gw.Services = b.Build()
	return gw
}

// acmRegistrationInventory is the inventory the ACM registration declares to
// a registry, keyed as AWSOperationInventory expects.
func acmRegistrationInventory() map[string]dispatch.Inventory {
	return withACM(&pipeline.GatewayConfig{}, acmawsapi.Deps{}).Services.Inventory()
}
