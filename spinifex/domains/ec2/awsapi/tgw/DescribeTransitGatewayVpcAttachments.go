package tgw

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2tgw "github.com/mulgadc/spinifex/spinifex/domains/ec2/tgw"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// DescribeTransitGatewayVpcAttachments returns accountID's VPC attachments.
func DescribeTransitGatewayVpcAttachments(ctx context.Context, input *ec2.DescribeTransitGatewayVpcAttachmentsInput, natsConn *nats.Conn, accountID string) (ec2.DescribeTransitGatewayVpcAttachmentsOutput, error) {
	var output ec2.DescribeTransitGatewayVpcAttachmentsOutput
	if input == nil {
		return output, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	result, err := ec2tgw.NewNATSTransitGatewayService(natsConn).DescribeTransitGatewayVpcAttachments(ctx, input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
