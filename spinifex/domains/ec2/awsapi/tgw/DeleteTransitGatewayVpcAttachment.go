package tgw

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2tgw "github.com/mulgadc/spinifex/spinifex/domains/ec2/tgw"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// DeleteTransitGatewayVpcAttachment forwards the action to the Transit Gateway service scoped to accountID.
func DeleteTransitGatewayVpcAttachment(ctx context.Context, input *ec2.DeleteTransitGatewayVpcAttachmentInput, natsConn *nats.Conn, accountID string) (ec2.DeleteTransitGatewayVpcAttachmentOutput, error) {
	var output ec2.DeleteTransitGatewayVpcAttachmentOutput
	if input == nil {
		return output, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	result, err := ec2tgw.NewNATSTransitGatewayService(natsConn).DeleteTransitGatewayVpcAttachment(ctx, input, accountID)
	if err != nil {
		return output, err
	}
	return *result, nil
}
