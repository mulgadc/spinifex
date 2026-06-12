package gateway_ec2_vpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// ValidateDetachNetworkInterfaceInput validates DetachNetworkInterface input.
func ValidateDetachNetworkInterfaceInput(input *ec2.DetachNetworkInterfaceInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.AttachmentId == nil || *input.AttachmentId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	return nil
}

// DetachNetworkInterface resolves the owning instance via DescribeNetworkInterfaces,
// then dispatches the detach command to that daemon.
func DetachNetworkInterface(input *ec2.DetachNetworkInterfaceInput, natsConn *nats.Conn, accountID string) (ec2.DetachNetworkInterfaceOutput, error) {
	var output ec2.DetachNetworkInterfaceOutput

	if err := ValidateDetachNetworkInterfaceInput(input); err != nil {
		return output, err
	}

	attachmentID := *input.AttachmentId
	force := false
	if input.Force != nil {
		force = *input.Force
	}

	instanceID, err := resolveAttachmentInstance(natsConn, accountID, attachmentID)
	if err != nil {
		return output, err
	}

	command := types.EC2InstanceCommand{
		ID:         instanceID,
		Attributes: types.EC2CommandAttributes{DetachENI: true},
		DetachENIData: &types.DetachENIData{
			AttachmentID: attachmentID,
			Force:        force,
		},
	}

	jsonData, err := json.Marshal(command)
	if err != nil {
		slog.Error("DetachNetworkInterface: marshal command failed", "err", err)
		return output, errors.New(awserrors.ErrorServerInternal)
	}

	reqMsg := nats.NewMsg(fmt.Sprintf("ec2.cmd.%s", instanceID))
	reqMsg.Data = jsonData
	reqMsg.Header.Set(utils.AccountIDHeader, accountID)
	msg, err := natsConn.RequestMsg(reqMsg, 30*time.Second)
	if err != nil {
		slog.Error("DetachNetworkInterface: NATS request failed",
			"instanceId", instanceID, "attachmentId", attachmentID, "err", err)
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
		slog.Error("DetachNetworkInterface: unmarshal response failed", "err", err)
		return output, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("DetachNetworkInterface completed",
		"instanceId", instanceID, "attachmentId", attachmentID)
	return output, nil
}

// resolveAttachmentInstance fans out DescribeNetworkInterfaces to find the instance
// owning attachmentID; returns InvalidAttachmentID.NotFound when no match.
func resolveAttachmentInstance(natsConn *nats.Conn, accountID, attachmentID string) (string, error) {
	input := &ec2.DescribeNetworkInterfacesInput{}
	reqData, err := json.Marshal(input)
	if err != nil {
		return "", errors.New(awserrors.ErrorServerInternal)
	}
	reqMsg := nats.NewMsg("ec2.DescribeNetworkInterfaces")
	reqMsg.Data = reqData
	reqMsg.Header.Set(utils.AccountIDHeader, accountID)
	msg, err := natsConn.RequestMsg(reqMsg, 10*time.Second)
	if err != nil {
		return "", errors.New(awserrors.ErrorServerInternal)
	}
	if _, err := utils.ValidateErrorPayload(msg.Data); err != nil {
		return "", errors.New(awserrors.ErrorInvalidAttachmentIDNotFound)
	}
	var desc ec2.DescribeNetworkInterfacesOutput
	if err := json.Unmarshal(msg.Data, &desc); err != nil {
		return "", errors.New(awserrors.ErrorServerInternal)
	}
	for _, ni := range desc.NetworkInterfaces {
		if ni.Attachment == nil || ni.Attachment.AttachmentId == nil {
			continue
		}
		if *ni.Attachment.AttachmentId != attachmentID {
			continue
		}
		if ni.Attachment.InstanceId == nil || *ni.Attachment.InstanceId == "" {
			return "", errors.New(awserrors.ErrorInvalidAttachmentIDNotFound)
		}
		return *ni.Attachment.InstanceId, nil
	}
	return "", errors.New(awserrors.ErrorInvalidAttachmentIDNotFound)
}
