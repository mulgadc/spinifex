package gateway

import (
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	operatoradmin "github.com/mulgadc/spinifex/spinifex/operator/admin"
)

// spinifexAdminActions is the set of Query actions the spinifex registration
// serves, which the characterization tests walk.
var spinifexAdminActions = func() map[string]bool {
	m := map[string]bool{}
	for _, name := range operatoradmin.ActionNames() {
		m[name] = true
	}
	return m
}()

// withOperator registers the spinifex Query service on gw from the fields gw
// holds, as awsgw composes it.
func withOperator(gw *GatewayConfig) *GatewayConfig {
	b := dispatch.NewBuilder()
	deps := operatoradmin.Deps{
		Version:     gw.Version,
		Commit:      gw.Commit,
		NATS:        gw.NATSConn,
		RootCAs:     gw.RootCAs,
		ActiveNodes: gw.DiscoverActiveNodes,
		Access:      gw.BedrockAccess,

		IAM:           gw.IAMService,
		Quota:         gw.Quota,
		ExpectedNodes: gw.ExpectedNodes,
		NodeIDs:       gw.NodeIDs,
	}
	// A nil store pointer stored in an interface is not a nil interface, so the
	// optional capabilities are only set when the gateway holds them.
	if gw.BedrockAccessAdmin != nil {
		deps.ModelAccess = gw.BedrockAccessAdmin
	}
	if gw.BedrockCredentials != nil {
		deps.Credentials = gw.BedrockCredentials
	}
	reg := operatoradmin.NewRegistration(deps)
	if err := b.Register(reg); err != nil {
		panic(err)
	}
	gw.Services = b.Build()
	return gw
}
