package awsgw_test

import (
	acmawsapi "github.com/mulgadc/spinifex/spinifex/domains/acm/awsapi"
	"github.com/mulgadc/spinifex/spinifex/gateway"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

// withACM registers the ACM control plane on gw backed by deps, as the awsgw
// role composes it. An unset deps.NATS takes gw.NATSConn.
func withACM(gw *gateway.GatewayConfig, deps acmawsapi.Deps) *gateway.GatewayConfig {
	if deps.NATS == nil {
		deps.NATS = gw.NATSConn
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
	return withACM(&gateway.GatewayConfig{}, acmawsapi.Deps{}).Services.Inventory()
}
