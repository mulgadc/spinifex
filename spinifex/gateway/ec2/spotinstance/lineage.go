package gateway_ec2_spotinstance

import (
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

const (
	// spotLineageRetries bounds the wait for the owning daemon to subscribe
	// ec2.cmd.{id}: it subscribes only after responding with the reservation, so
	// the write-back can briefly race ahead and see no responder.
	spotLineageRetries    = 10
	spotLineageRetryDelay = 100 * time.Millisecond
	spotLineageReqTimeout = 5 * time.Second
)

// stampSpotLineage writes the spot lineage (InstanceLifecycle=spot +
// SpotInstanceRequestId) back to each launched VM via its owner subject. It is
// best-effort: the VMs are already running and the SIRs persisted, so a
// persistent miss is logged and the request still succeeds — lineage is a
// projection, never a launch precondition. Requests with no mapped instance
// (no VM launched for them) are skipped.
func stampSpotLineage(natsConn *nats.Conn, requests []*ec2.SpotInstanceRequest, accountID string) {
	for _, req := range requests {
		if req == nil {
			continue
		}
		instanceID := aws.StringValue(req.InstanceId)
		sirID := aws.StringValue(req.SpotInstanceRequestId)
		if instanceID == "" || sirID == "" {
			continue
		}
		if err := sendSpotLineageCommand(natsConn, instanceID, sirID, accountID); err != nil {
			slog.Warn("RequestSpotInstances: spot lineage write-back failed",
				"instance_id", instanceID, "sir_id", sirID, "err", err)
		}
	}
}

// sendSpotLineageCommand sends the SetSpotLineage command to the instance's owner
// via ec2.cmd.{id}, retrying past nats.ErrNoResponders until the owner subscribes.
// A non-no-responders transport error or an owner-returned error code stops the
// retry and is surfaced to the caller.
func sendSpotLineageCommand(natsConn *nats.Conn, instanceID, sirID, accountID string) error {
	command := types.EC2InstanceCommand{
		ID:              instanceID,
		Attributes:      types.EC2CommandAttributes{SetSpotLineage: true},
		SpotLineageData: &types.SpotLineageData{SpotInstanceRequestId: sirID},
	}
	jsonData, err := json.Marshal(command)
	if err != nil {
		return err
	}

	var lastErr error
	for attempt := range spotLineageRetries {
		reqMsg := nats.NewMsg("ec2.cmd." + instanceID)
		reqMsg.Data = jsonData
		reqMsg.Header.Set(utils.AccountIDHeader, accountID)

		msg, err := natsConn.RequestMsg(reqMsg, spotLineageReqTimeout)
		if err == nil {
			if responseError, parseErr := utils.ValidateErrorPayload(msg.Data); parseErr != nil {
				return errors.New(*responseError.Code)
			}
			return nil
		}
		if !errors.Is(err, nats.ErrNoResponders) {
			return err
		}
		lastErr = err
		if attempt < spotLineageRetries-1 {
			time.Sleep(spotLineageRetryDelay)
		}
	}
	return lastErr
}
