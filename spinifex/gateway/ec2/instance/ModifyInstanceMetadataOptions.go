package gateway_ec2_instance

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/utils"
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
	return utils.NATSRequestCtx[ec2.ModifyInstanceMetadataOptionsOutput](ctx, natsConn, "ec2.ModifyInstanceMetadataOptions", input, 30*time.Second, accountID)
}
