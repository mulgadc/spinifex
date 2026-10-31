package gateway

import (
	"net/http"

	stsawsapi "github.com/mulgadc/spinifex/spinifex/domains/sts/awsapi"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

// stsRegistration is the STS registration over the IAMService and STSService
// gw holds, as the awsgw role composes it. Set them on gw first.
func stsRegistration(gw *GatewayConfig) dispatch.Registration {
	return stsawsapi.NewRegistration(stsawsapi.Deps{STS: gw.STSService, IAM: gw.IAMService})
}

// withSTS registers STS alone on gw and composes its unsigned web-identity
// capability, as awsgw does.
func withSTS(gw *GatewayConfig) *GatewayConfig {
	gw.AnonymousSTS = stsawsapi.NewAnonymousAssumeRoleWithWebIdentity(stsawsapi.Deps{STS: gw.STSService, IAM: gw.IAMService})
	return withServices(gw, stsRegistration(gw))
}

// withIAMAndSTS registers IAM and STS together on gw, with the unsigned
// web-identity capability, as awsgw composes them.
func withIAMAndSTS(gw *GatewayConfig) *GatewayConfig {
	gw.AnonymousSTS = stsawsapi.NewAnonymousAssumeRoleWithWebIdentity(stsawsapi.Deps{STS: gw.STSService, IAM: gw.IAMService})
	return withServices(gw, iamRegistration(gw), stsRegistration(gw))
}

// STS_Request dispatches r to the registered STS service exactly as Request
// does, registering it first from the fields gw holds when the test has not.
func (gw *GatewayConfig) STS_Request(w http.ResponseWriter, r *http.Request) error {
	if gw.Services == nil {
		withSTS(gw)
	}
	entry, ok := gw.registered(stsawsapi.ServiceName)
	if !ok {
		panic("STS is not registered on this gateway")
	}
	return gw.dispatchRegistered(entry, w, r)
}
