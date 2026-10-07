package gateway

import (
	"net/http"

	awsapi "github.com/mulgadc/spinifex/spinifex/domains/ecr/awsapi"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

// withECR registers the ECR control plane on gw backed by deps, as the awsgw
// role composes it. An unset deps.NATS takes gw.NATSConn.
func withECR(gw *GatewayConfig, deps awsapi.Deps) *GatewayConfig {
	if deps.NATS == nil {
		deps.NATS = gw.NATSConn
	}
	b := dispatch.NewBuilder()
	if err := b.Register(awsapi.NewRegistration(deps)); err != nil {
		panic(err)
	}
	gw.Services = b.Build()
	return gw
}

// serveECR dispatches r to gw's registered ECR service exactly as Request does
// past its NATS gate, first registering one with no capabilities if needed.
func (gw *GatewayConfig) serveECR(w http.ResponseWriter, r *http.Request) error {
	entry, ok := gw.registered(awsapi.ServiceName)
	if !ok {
		withECR(gw, awsapi.Deps{})
		entry, _ = gw.registered(awsapi.ServiceName)
	}
	return gw.dispatchRegistered(entry, w, r)
}

// ecrRegistrationInventory is the inventory the ECR registration declares to
// a registry, keyed as AWSOperationInventory expects.
func ecrRegistrationInventory() map[string]dispatch.Inventory {
	return withECR(&GatewayConfig{}, awsapi.Deps{}).Services.Inventory()
}
