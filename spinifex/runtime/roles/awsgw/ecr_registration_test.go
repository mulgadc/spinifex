package awsgw_test

import (
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	"github.com/mulgadc/spinifex/spinifex/gateway"
	"maps"

	awsapi "github.com/mulgadc/spinifex/spinifex/domains/ecr/awsapi"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

// withECR registers the ECR control plane on gw backed by deps, as the awsgw
// role composes it. An unset deps.NATS takes gw.NATSConn.
func withECR(gw *gateway.GatewayConfig, deps awsapi.Deps) *gateway.GatewayConfig {
	if deps.NATS == nil {
		deps.NATS = gwfix.DepsOf(gw).NATS
	}
	b := dispatch.NewBuilder()
	if err := b.Register(awsapi.NewRegistration(deps)); err != nil {
		panic(err)
	}
	gw.Services = b.Build()
	return gw
}

// ecrRegistrationInventory is the inventory the ECR registration declares to
// a registry, keyed as AWSOperationInventory expects.
func ecrRegistrationInventory() map[string]dispatch.Inventory {
	inventory := withECR(&gateway.GatewayConfig{}, awsapi.Deps{}).Services.Inventory()
	maps.Copy(inventory, withContainers(&gateway.GatewayConfig{}).Services.Inventory())
	return inventory
}
