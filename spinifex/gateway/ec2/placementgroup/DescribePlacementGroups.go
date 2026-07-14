package gateway_ec2_placementgroup

import (
	"context"
	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_ec2_placementgroup "github.com/mulgadc/spinifex/spinifex/handlers/ec2/placementgroup"
	"github.com/nats-io/nats.go"
)

// DescribePlacementGroups handles the EC2 DescribePlacementGroups API call.
func DescribePlacementGroups(ctx context.Context, input *ec2.DescribePlacementGroupsInput, natsConn *nats.Conn, accountID string) (ec2.DescribePlacementGroupsOutput, error) {
	var output ec2.DescribePlacementGroupsOutput

	svc := handlers_ec2_placementgroup.NewNATSPlacementGroupService(natsConn)
	result, err := svc.DescribePlacementGroups(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
