package gateway_ec2_vpc

import (
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

// ValidateAttachNetworkInterfaceInput validates the input parameters.
// NetworkInterfaceId, InstanceId, and DeviceIndex are required.
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

// AttachNetworkInterface routes the AWS EC2 AttachNetworkInterface call
// to the daemon that owns the target instance via the per-instance
// ec2.cmd.{instanceID} channel. The daemon runs both the KV-side
// AttachENI and the live QMP hot-plug pipeline.
func AttachNetworkInterface(input *ec2.AttachNetworkInterfaceInput, natsConn *nats.Conn, accountID string) (ec2.AttachNetworkInterfaceOutput, error) {
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
		slog.Error("AttachNetworkInterface: marshal command failed", "err", err)
		return output, errors.New(awserrors.ErrorServerInternal)
	}

	reqMsg := nats.NewMsg(fmt.Sprintf("ec2.cmd.%s", instanceID))
	reqMsg.Data = jsonData
	reqMsg.Header.Set(utils.AccountIDHeader, accountID)
	msg, err := natsConn.RequestMsg(reqMsg, 30*time.Second)
	if err != nil {
		slog.Error("AttachNetworkInterface: NATS request failed",
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
		slog.Error("AttachNetworkInterface: unmarshal response failed", "err", err)
		return output, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("AttachNetworkInterface completed",
		"instanceId", instanceID, "eniId", eniID,
		"attachmentId", aws.StringValue(output.AttachmentId))
	return output, nil
}
