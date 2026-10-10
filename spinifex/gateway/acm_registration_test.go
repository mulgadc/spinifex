package gateway

import (
	"net/http"

	acmawsapi "github.com/mulgadc/spinifex/spinifex/domains/acm/awsapi"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

// withACM registers the ACM control plane on gw backed by deps, as the awsgw
// role composes it. An unset deps.NATS takes gw.NATSConn.
func withACM(gw *GatewayConfig, deps acmawsapi.Deps) *GatewayConfig {
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

// serveACM dispatches r to gw's registered ACM service exactly as Request does
// past its NATS gate, first registering one with no capabilities if needed.
func (gw *GatewayConfig) serveACM(w http.ResponseWriter, r *http.Request) error {
	entry, ok := gw.registered(acmawsapi.ServiceName)
	if !ok {
		withACM(gw, acmawsapi.Deps{})
		entry, _ = gw.registered(acmawsapi.ServiceName)
	}
	return gw.dispatchRegistered(entry, w, r)
}

// acmRegistrationInventory is the inventory the ACM registration declares to
// a registry, keyed as AWSOperationInventory expects.
func acmRegistrationInventory() map[string]dispatch.Inventory {
	return withACM(&GatewayConfig{}, acmawsapi.Deps{}).Services.Inventory()
}
