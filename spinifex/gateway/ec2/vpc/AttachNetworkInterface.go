package gateway_ec2_vpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// ValidateAttachNetworkInterfaceInput validates AttachNetworkInterface input.
func ValidateAttachNetworkInterfaceInput(input *ec2.AttachNetworkInterfaceInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.NetworkInterfaceId == nil || *input.NetworkInterfaceId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if input.InstanceId == nil || *input.InstanceId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if input.DeviceIndex == nil {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// AttachNetworkInterface dispatches to the owning daemon via ec2.cmd.{instanceID};
// the daemon handles both KV-side AttachENI and QMP hot-plug.
func AttachNetworkInterface(ctx context.Context, input *ec2.AttachNetworkInterfaceInput, natsConn *nats.Conn, accountID string) (ec2.AttachNetworkInterfaceOutput, error) {
	var output ec2.AttachNetworkInterfaceOutput

	if err := ValidateAttachNetworkInterfaceInput(input); err != nil {
		return output, err
	}

	instanceID := *input.InstanceId
	eniID := *input.NetworkInterfaceId
	deviceIndex := *input.DeviceIndex

	command := types.EC2InstanceCommand{
		ID:         instanceID,
		Attributes: types.EC2CommandAttributes{AttachENI: true},
		AttachENIData: &types.AttachENIData{
			NetworkInterfaceID: eniID,
			DeviceIndex:        deviceIndex,
		},
	}

	jsonData, err := json.Marshal(command)
	if err != nil {
		slog.ErrorContext(ctx, "AttachNetworkInterface: marshal command failed", "err", err)
		return output, errors.New(awserrors.ErrorServerInternal)
	}

	reqMsg := nats.NewMsg(fmt.Sprintf("ec2.cmd.%s", instanceID))
	reqMsg.Data = jsonData
	reqMsg.Header.Set(utils.AccountIDHeader, accountID)
	utils.InjectTraceContext(ctx, reqMsg.Header)
	msg, err := natsConn.RequestMsg(reqMsg, 30*time.Second)
	if err != nil {
		slog.ErrorContext(ctx, "AttachNetworkInterface: NATS request failed",
			"instanceId", instanceID, "eniId", eniID, "err", err)
		if errors.Is(err, nats.ErrNoResponders) {
			return output, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
		}
		return output, errors.New(awserrors.ErrorServerInternal)
	}

	responseError, err := utils.ValidateErrorPayload(msg.Data)
	if err != nil {
		return output, errors.New(*responseError.Code)
	}

	if err := json.Unmarshal(msg.Data, &output); err != nil {
		slog.ErrorContext(ctx, "AttachNetworkInterface: unmarshal response failed", "err", err)
		return output, errors.New(awserrors.ErrorServerInternal)
	}

	slog.InfoContext(ctx, "AttachNetworkInterface completed",
		"instanceId", instanceID, "eniId", eniID,
		"attachmentId", aws.StringValue(output.AttachmentId))
	return output, nil
}
