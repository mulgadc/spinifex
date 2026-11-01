package awsgw_test

import (
	"context"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/pipeline"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw"

	ec2instanceapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/instance"
	iamawsapi "github.com/mulgadc/spinifex/spinifex/domains/iam/awsapi"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

// iamRegistration is the IAM registration over the IAMService, NATS connection
// and node count gw holds, as the awsgw role composes it. Set them on gw first.
func iamRegistration(gw *pipeline.GatewayConfig) dispatch.Registration {
	return iamawsapi.NewRegistration(iamawsapi.Deps{
		IAM: gwfix.DepsOf(gw).IAM,
		CountInstanceProfileAssociations: func(ctx context.Context, accountID, profileARN string) (int, error) {
			return ec2instanceapi.CountInstanceProfileAssociations(ctx, gwfix.DepsOf(gw).NATS, (&awsgw.NodeDiscovery{NATS: gwfix.DepsOf(gw).NATS, Expected: gwfix.DepsOf(gw).Nodes}).ActiveNodes(ctx), accountID, profileARN)
		},
	})
}
