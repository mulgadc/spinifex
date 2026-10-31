package awsgw_test

import (
	"github.com/mulgadc/spinifex/spinifex/gateway"

	stsawsapi "github.com/mulgadc/spinifex/spinifex/domains/sts/awsapi"
)

// withSTS registers STS alone on gw and composes its unsigned web-identity
// capability, as awsgw does.
func withSTS(gw *gateway.GatewayConfig) *gateway.GatewayConfig {
	gw.AnonymousSTS = stsawsapi.NewAnonymousAssumeRoleWithWebIdentity(stsawsapi.Deps{STS: gw.STSService, IAM: gw.IAMService})
	return withServices(gw, stsRegistration(gw))
}
