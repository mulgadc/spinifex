package awsgw_test

import (
	"context"
	"github.com/mulgadc/spinifex/spinifex/gateway"

	ec2instanceapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/instance"
	iamawsapi "github.com/mulgadc/spinifex/spinifex/domains/iam/awsapi"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

// iamRegistration is the IAM registration over the IAMService, NATS connection
// and node count gw holds, as the awsgw role composes it. Set them on gw first.
func iamRegistration(gw *gateway.GatewayConfig) dispatch.Registration {
	return iamawsapi.NewRegistration(iamawsapi.Deps{
		IAM: gw.IAMService,
		CountInstanceProfileAssociations: func(ctx context.Context, accountID, profileARN string) (int, error) {
			return ec2instanceapi.CountInstanceProfileAssociations(ctx, gw.NATSConn, gw.DiscoverActiveNodes(ctx), accountID, profileARN)
		},
	})
}
