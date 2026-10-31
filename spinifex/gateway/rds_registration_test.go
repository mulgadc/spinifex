package gateway

import (
	"net/http"

	rdsawsapi "github.com/mulgadc/spinifex/spinifex/domains/rds/awsapi"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

// withRDS registers RDS on gw with the NATS connection and node count it holds,
// as the awsgw role composes it. Set both on gw before calling.
func withRDS(gw *GatewayConfig) *GatewayConfig {
	deps := rdsawsapi.Deps{NATS: gw.NATSConn, ExpectedNodes: gw.ExpectedNodes}
	if gw.Quota != nil {
		deps.Quota = gw.Quota
	}
	b := dispatch.NewBuilder()
	if err := b.Register(rdsawsapi.NewRegistration(deps)); err != nil {
		panic(err)
	}
	gw.Services = b.Build()
	return gw
}

// serveRDS dispatches r to the registered RDS service exactly as Request does,
// registering it first from the fields gw holds when the test has not.
func (gw *GatewayConfig) serveRDS(w http.ResponseWriter, r *http.Request) error {
	if gw.Services == nil {
		withRDS(gw)
	}
	entry, ok := gw.registered(rdsawsapi.ServiceName)
	if !ok {
		panic("RDS is not registered on this gateway")
	}
	return gw.dispatchRegistered(entry, w, r)
}

// rdsRegistrationInventory is the inventory the RDS registration declares to a
// registry, keyed as AWSOperationInventory expects.
func rdsRegistrationInventory() map[string]dispatch.Inventory {
	return withRDS(&GatewayConfig{}).Services.Inventory()
}
