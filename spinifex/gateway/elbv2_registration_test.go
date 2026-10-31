package gateway

import (
	"net/http"

	elbv2awsapi "github.com/mulgadc/spinifex/spinifex/domains/elbv2/awsapi"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

// withELBv2 registers ELBv2 on gw with the NATS connection and quota it holds,
// as the awsgw role composes it. Set both on gw before calling.
func withELBv2(gw *GatewayConfig) *GatewayConfig {
	b := dispatch.NewBuilder()
	reg := elbv2awsapi.NewRegistration(elbv2awsapi.Deps{NATS: gw.NATSConn, Quota: quotaFor(gw)})
	if err := b.Register(reg); err != nil {
		panic(err)
	}
	gw.Services = b.Build()
	return gw
}

// serveELBv2 dispatches r to the registered ELBv2 service exactly as Request
// does past its NATS gate, registering it first from the fields gw holds when
// the test has not.
func (gw *GatewayConfig) serveELBv2(w http.ResponseWriter, r *http.Request) error {
	if gw.Services == nil {
		withELBv2(gw)
	}
	entry, ok := gw.Registered(elbv2awsapi.ServiceName)
	if !ok {
		panic("ELBv2 is not registered on this gateway")
	}
	return gw.dispatchRegistered(entry, w, r)
}

// elbv2ServedActions lists every action the ELBv2 service dispatches.
func elbv2ServedActions() []string {
	return elbv2awsapi.ActionNames()
}
