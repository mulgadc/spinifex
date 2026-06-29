package daemon

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_instance "github.com/mulgadc/spinifex/spinifex/handlers/ec2/instance"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	spxtypes "github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// Compile-time check that Daemon satisfies the EKS worker-launch surface.
var _ handlers_eks.WorkerLauncher = (*Daemon)(nil)

// RunWorkerInstance launches a nodegroup worker via the normal RunInstances path.
// UserData is base64-encoded before forwarding; the worker is customer-visible.
func (d *Daemon) RunWorkerInstance(input *ec2.RunInstancesInput, accountID string) (*ec2.Reservation, error) {
	if d.instanceService == nil {
		return nil, errors.New("eks worker: instance service not initialized")
	}
	if input == nil {
		return nil, errors.New("eks worker: nil RunInstancesInput")
	}
	if input.UserData != nil && *input.UserData != "" {
		input.UserData = aws.String(base64.StdEncoding.EncodeToString([]byte(*input.UserData)))
	}
	return d.instanceService.RunInstances(input, accountID)
}

// TerminateWorkerInstances terminates nodegroup workers by routing a terminate
// command to whichever node owns each VM (per-instance ec2.cmd.<id>), mirroring
// the gateway TerminateInstances path. The owning daemon runs the full teardown
// — detach + force-delete the worker's primary ENI and clear its
// spinifex-vpc-enis record — so a dangling in-use ENI cannot pin the customer
// VPC/subnet undeletable (mulga-siv-408). A local-only vmMgr.Get would skip any
// worker placed on another node, stranding both the VM and its ENI. An instance
// with no owner (no responder, not found) is already gone, so a retried
// DeleteNodegroup stays idempotent.
func (d *Daemon) TerminateWorkerInstances(instanceIDs []string, accountID string) error {
	if d.natsConn == nil {
		return errors.New("eks worker: NATS connection not initialized")
	}
	var errs []error
	for _, id := range instanceIDs {
		if id == "" {
			continue
		}
		if err := d.terminateWorkerInstance(id, accountID); err != nil {
			errs = append(errs, fmt.Errorf("terminate worker %s: %w", id, err))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// terminateWorkerInstance routes a single worker terminate to its owning node.
// StopInstance is set alongside TerminateInstance so the owner does not restart
// it. A NoResponders reply means no daemon owns the instance: it is already
// gone, which a retried teardown treats as success. A NotFound error payload
// from the owner is likewise idempotent.
func (d *Daemon) terminateWorkerInstance(instanceID, accountID string) error {
	cmd := spxtypes.EC2InstanceCommand{
		ID: instanceID,
		Attributes: spxtypes.EC2CommandAttributes{
			StopInstance:      true,
			TerminateInstance: true,
		},
	}
	data, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshal terminate command: %w", err)
	}

	subject := fmt.Sprintf("ec2.cmd.%s", instanceID)
	var msg *nats.Msg
	for attempt := range 3 {
		reqMsg := nats.NewMsg(subject)
		reqMsg.Data = data
		reqMsg.Header.Set(utils.AccountIDHeader, accountID)
		msg, err = d.natsConn.RequestMsg(reqMsg, 5*time.Second)
		if err == nil || !errors.Is(err, nats.ErrNoResponders) {
			break
		}
		if attempt < 2 {
			time.Sleep(time.Duration(attempt+1) * time.Second)
		}
	}
	if errors.Is(err, nats.ErrNoResponders) {
		// No live ec2.cmd owner: the worker is stopped — its per-instance
		// subscription is torn down at stop, including a stopped+wedged VM whose
		// volume remount broke. Fall back to the ec2.terminate queue group, which
		// runs TerminateStoppedInstance against shared KV (deleting the worker's
		// volumes, IP, and ENI) so the ENI cannot pin the customer subnet/VPC
		// undeletable. Without this a DeleteCluster wedges in DELETING on
		// DependencyViolation until the operator manually terminates the node.
		return d.terminateStoppedWorker(instanceID, accountID)
	}
	if err != nil {
		return err
	}
	if errPayload, parseErr := utils.ValidateErrorPayload(msg.Data); parseErr != nil {
		if *errPayload.Code == awserrors.ErrorInvalidInstanceIDNotFound {
			slog.Debug("TerminateWorkerInstances: owner reports instance gone, idempotent", "instanceId", instanceID)
			return nil
		}
		return errors.New(*errPayload.Code)
	}
	return nil
}

// terminateStoppedWorker tears down a worker that has no live ec2.cmd owner via
// the ec2.terminate queue group, mirroring the gateway TerminateInstances
// fallback. Any worker daemon services it from shared KV, so a stopped (incl.
// stopped+wedged) worker is reaped regardless of which node ran the teardown. A
// NotFound payload — or no responder at all — means the instance is already
// gone, which a retried teardown treats as idempotent success.
func (d *Daemon) terminateStoppedWorker(instanceID, accountID string) error {
	req, err := json.Marshal(handlers_ec2_instance.TerminateStoppedInstanceInput{InstanceID: instanceID})
	if err != nil {
		return fmt.Errorf("marshal stopped-terminate request: %w", err)
	}
	reqMsg := nats.NewMsg("ec2.terminate")
	reqMsg.Data = req
	reqMsg.Header.Set(utils.AccountIDHeader, accountID)
	msg, err := d.natsConn.RequestMsg(reqMsg, 30*time.Second)
	if errors.Is(err, nats.ErrNoResponders) {
		slog.Debug("TerminateWorkerInstances: no ec2.terminate responder, instance already gone", "instanceId", instanceID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("ec2.terminate stopped worker %s: %w", instanceID, err)
	}
	if errPayload, parseErr := utils.ValidateErrorPayload(msg.Data); parseErr != nil {
		if *errPayload.Code == awserrors.ErrorInvalidInstanceIDNotFound {
			slog.Debug("TerminateWorkerInstances: stopped worker already gone, idempotent", "instanceId", instanceID)
			return nil
		}
		return errors.New(*errPayload.Code)
	}
	slog.Info("TerminateWorkerInstances: terminated stopped worker via ec2.terminate", "instanceId", instanceID)
	return nil
}
