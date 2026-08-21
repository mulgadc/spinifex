package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// monitorInstances moves instances to the 60s detailed telemetry tier.
func (d *Daemon) monitorInstances(ctx context.Context, input *ec2.MonitorInstancesInput, accountID string) (*ec2.MonitorInstancesOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	monitorings, err := d.setInstanceMonitoring(ctx, input.InstanceIds, true, accountID)
	if err != nil {
		return nil, err
	}
	return &ec2.MonitorInstancesOutput{InstanceMonitorings: monitorings}, nil
}

// unmonitorInstances returns instances to the 300s basic telemetry tier.
func (d *Daemon) unmonitorInstances(ctx context.Context, input *ec2.UnmonitorInstancesInput, accountID string) (*ec2.UnmonitorInstancesOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	monitorings, err := d.setInstanceMonitoring(ctx, input.InstanceIds, false, accountID)
	if err != nil {
		return nil, err
	}
	return &ec2.UnmonitorInstancesOutput{InstanceMonitorings: monitorings}, nil
}

// setInstanceMonitoring applies the tier to each instance through its owner
// concurrently, so a many-instance call costs one owner timeout rather than
// one per instance. The first failure in input order is returned.
func (d *Daemon) setInstanceMonitoring(ctx context.Context, instanceIDs []*string, enabled bool, accountID string) ([]*ec2.InstanceMonitoring, error) {
	if len(instanceIDs) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	errs := make([]error, len(instanceIDs))
	var wg sync.WaitGroup
	for i, id := range instanceIDs {
		if id == nil {
			errs[i] = errors.New(awserrors.ErrorInvalidInstanceIDMalformed)
			continue
		}
		wg.Go(func() {
			errs[i] = d.setOneInstanceMonitoring(ctx, *id, enabled, accountID)
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}

	// Every instance applied, so they all report the requested tier. Built
	// after the error sweep so a partial failure returns no state at all.
	state := ec2.MonitoringStateDisabled
	if enabled {
		state = ec2.MonitoringStateEnabled
	}
	monitorings := make([]*ec2.InstanceMonitoring, 0, len(instanceIDs))
	for _, id := range instanceIDs {
		monitorings = append(monitorings, &ec2.InstanceMonitoring{
			InstanceId: aws.String(*id),
			Monitoring: &ec2.Monitoring{State: aws.String(state)},
		})
	}
	return monitorings, nil
}

// setOneInstanceMonitoring asks the owning daemon over ec2.cmd.<id> to apply
// the tier; only the owner subscribes there. No responders means the instance
// isn't running, so it falls back to the shared stopped store; a timeout
// (partitioned-but-subscribed owner) surfaces InvalidID.NotFound.
func (d *Daemon) setOneInstanceMonitoring(ctx context.Context, instanceID string, enabled bool, accountID string) error {
	command := types.EC2InstanceCommand{
		ID:                     instanceID,
		Attributes:             types.EC2CommandAttributes{SetInstanceMonitoring: true},
		InstanceMonitoringData: &types.InstanceMonitoringData{Enabled: enabled},
	}
	body, err := json.Marshal(command)
	if err != nil {
		slog.ErrorContext(ctx, "setInstanceMonitoring: failed to marshal command", "instanceId", instanceID, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	reqMsg := nats.NewMsg("ec2.cmd." + instanceID)
	reqMsg.Data = body
	reqMsg.Header.Set(utils.AccountIDHeader, accountID)
	utils.InjectTraceContext(ctx, reqMsg.Header)

	msg, err := d.natsConn.RequestMsg(reqMsg, instanceOwnerCommandTimeout)
	switch {
	case errors.Is(err, nats.ErrNoResponders):
		return d.instanceService.SetStoppedInstanceMonitoring(ctx, instanceID, enabled, accountID)
	case errors.Is(err, nats.ErrTimeout):
		return errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
	case err != nil:
		slog.ErrorContext(ctx, "setInstanceMonitoring: owner request failed", "instanceId", instanceID, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	if responseError, parseErr := utils.ValidateErrorPayload(msg.Data); parseErr != nil {
		return errors.New(*responseError.Code)
	}
	return nil
}
