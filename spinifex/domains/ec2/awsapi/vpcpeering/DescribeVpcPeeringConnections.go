package vpcpeering

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2vpcpeering "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpcpeering"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// DescribeVpcPeeringConnections returns the connections accountID can see.
func DescribeVpcPeeringConnections(ctx context.Context, input *ec2.DescribeVpcPeeringConnectionsInput, natsConn *nats.Conn, accountID string) (ec2.DescribeVpcPeeringConnectionsOutput, error) {
	var output ec2.DescribeVpcPeeringConnectionsOutput
	if input == nil {
		return output, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	result, err := ec2vpcpeering.NewNATSVpcPeeringService(natsConn).DescribeVpcPeeringConnections(ctx, input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
