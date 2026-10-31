package gateway

import (
	"context"
	"net/http"

	ec2instanceapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/instance"
	iamawsapi "github.com/mulgadc/spinifex/spinifex/domains/iam/awsapi"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

// iamRegistration is the IAM registration over the IAMService, NATS connection
// and node count gw holds, as the awsgw role composes it. Set them on gw first.
func iamRegistration(gw *GatewayConfig) dispatch.Registration {
	return iamawsapi.NewRegistration(iamawsapi.Deps{
		IAM: gw.IAMService,
		CountInstanceProfileAssociations: func(ctx context.Context, accountID, profileARN string) (int, error) {
			return ec2instanceapi.CountInstanceProfileAssociations(ctx, gw.NATSConn, gw.DiscoverActiveNodes(ctx), accountID, profileARN)
		},
	})
}

// withServices makes regs the gateway's registered services.
func withServices(gw *GatewayConfig, regs ...dispatch.Registration) *GatewayConfig {
	b := dispatch.NewBuilder()
	for _, reg := range regs {
		if err := b.Register(reg); err != nil {
			panic(err)
		}
	}
	gw.Services = b.Build()
	return gw
}

// withIAM registers IAM alone on gw.
func withIAM(gw *GatewayConfig) *GatewayConfig {
	return withServices(gw, iamRegistration(gw))
}

// IAM_Request dispatches r to the registered IAM service exactly as Request
// does, registering it first from the fields gw holds when the test has not.
func (gw *GatewayConfig) IAM_Request(w http.ResponseWriter, r *http.Request) error {
	if gw.Services == nil {
		withIAM(gw)
	}
	entry, ok := gw.registered(iamawsapi.ServiceName)
	if !ok {
		panic("IAM is not registered on this gateway")
	}
	return gw.dispatchRegistered(entry, w, r)
}
