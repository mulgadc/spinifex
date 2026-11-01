package awsgw_test

import (
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	"github.com/mulgadc/spinifex/spinifex/gateway"

	stsawsapi "github.com/mulgadc/spinifex/spinifex/domains/sts/awsapi"
)

// withSTS registers STS alone on gw and composes its unsigned web-identity
// capability, as awsgw does.
func withSTS(gw *gateway.GatewayConfig) *gateway.GatewayConfig {
	gw.PreAuth = append(gw.PreAuth, stsawsapi.NewPreAuth(stsawsapi.Deps{STS: gwfix.DepsOf(gw).STS, IAM: gwfix.DepsOf(gw).IAM}))
	return withServices(gw, stsRegistration(gw))
}
