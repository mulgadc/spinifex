package vpcpeering

import (
	"context"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	ec2vpcpeering "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpcpeering"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// AccepterVpcARN resolves the accepter VPC an Accept is also authorised
// against, as AWS does. It is "" when the caller does not accept a visible
// connection, which the handler then answers as not found.
func AccepterVpcARN(ctx context.Context, natsConn *nats.Conn, region, accountID string, input *ec2.AcceptVpcPeeringConnectionInput) (string, error) {
	id := aws.StringValue(input.VpcPeeringConnectionId)
	if id == "" {
		return "", nil
	}
	out, err := ec2vpcpeering.NewNATSVpcPeeringService(natsConn).DescribeVpcPeeringConnections(ctx,
		&ec2.DescribeVpcPeeringConnectionsInput{VpcPeeringConnectionIds: []*string{aws.String(id)}}, accountID)
	if err != nil {
		if code, _ := awserrors.ResolveErrorCode(err); code == awserrors.ErrorInvalidVpcPeeringConnectionIDNotFound {
			return "", nil
		}
		return "", err
	}
	for _, c := range out.VpcPeeringConnections {
		info := c.AccepterVpcInfo
		if aws.StringValue(c.VpcPeeringConnectionId) != id || info == nil || aws.StringValue(info.OwnerId) != accountID {
			continue
		}
		if vpcID := aws.StringValue(info.VpcId); vpcID != "" {
			return arn.FormatEC2(arn.EC2VPC, region, accountID, vpcID), nil
		}
	}
	return "", nil
}
