package instance

import (
	"context"
	"errors"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/contracts/ec2/v1"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// ModifyInstanceMetadataOptions validates the instance ID and round-trips the
// request to the owning daemon, which enforces the IMDSv2-only posture (only the
// hop limit is mutable) and returns the updated block.
func ModifyInstanceMetadataOptions(ctx context.Context, input *ec2.ModifyInstanceMetadataOptionsInput, natsConn *nats.Conn, accountID string) (*ec2.ModifyInstanceMetadataOptionsOutput, error) {
	if input == nil || input.InstanceId == nil || *input.InstanceId == "" {
		return nil, errors.New(awserrors.ErrorInvalidInstanceIDMalformed)
	}
	if !strings.HasPrefix(*input.InstanceId, "i-") {
		return nil, errors.New(awserrors.ErrorInvalidInstanceIDMalformed)
	}
	cmd := ec2v1.EC2InstanceCommand{
		ID:         *input.InstanceId,
		Attributes: ec2v1.EC2CommandAttributes{ModifyInstanceMetadataOptions: true},
		InstanceMetadataOptionsData: &ec2v1.InstanceMetadataOptionsData{
			HttpTokens:              aws.StringValue(input.HttpTokens),
			HttpEndpoint:            aws.StringValue(input.HttpEndpoint),
			HttpProtocolIpv6:        aws.StringValue(input.HttpProtocolIpv6),
			InstanceMetadataTags:    aws.StringValue(input.InstanceMetadataTags),
			HttpPutResponseHopLimit: input.HttpPutResponseHopLimit,
		},
	}
	return routeInstanceMutation[ec2.ModifyInstanceMetadataOptionsOutput](ctx, natsConn, *input.InstanceId, accountID, cmd, "ec2.ModifyInstanceMetadataOptions", input)
}
