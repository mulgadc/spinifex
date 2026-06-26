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
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	spxtypes "github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// Compile-time check that Daemon satisfies the EKS worker-launch surface.
var _ handlers_eks.WorkerLauncher = (*Daemon)(nil)

// RunWorkerInstance launches a nodegroup worker on the local node.
func (d *Daemon) RunWorkerInstance(input *ec2.RunInstancesInput, accountID string) (*ec2.Reservation, error) {
	return d.RunWorkerInstanceOnNode("", input, accountID)
}

// RunWorkerInstanceOnNode launches a nodegroup worker via the normal RunInstances
// path. UserData is base64-encoded before forwarding; the worker is
// customer-visible. A non-empty nodeID pins the worker to that host for nodegroup
// spread by publishing the node-targeted ec2.RunInstances.<type>.<node> request
// (the node stays subscribed even at capacity); an empty nodeID launches on the
// local node in process, preserving the original behaviour.
func (d *Daemon) RunWorkerInstanceOnNode(nodeID string, input *ec2.RunInstancesInput, accountID string) (*ec2.Reservation, error) {
	if input == nil {
		return nil, errors.New("eks worker: nil RunInstancesInput")
	}
	if input.UserData != nil && *input.UserData != "" {
		input.UserData = aws.String(base64.StdEncoding.EncodeToString([]byte(*input.UserData)))
	}
	if nodeID == "" {
		if d.instanceService == nil {
			return nil, errors.New("eks worker: instance service not initialized")
		}
		return d.instanceService.RunInstances(input, accountID)
	}
	if d.natsConn == nil {
		return nil, errors.New("eks worker: NATS connection not initialized")
	}
	subject := fmt.Sprintf("ec2.RunInstances.%s.%s", aws.StringValue(input.InstanceType), nodeID)
	return utils.NATSRequest[ec2.Reservation](d.natsConn, subject, input, 5*time.Minute, accountID)
}

// TerminateWorkerInstances terminates nodegroup workers by routing a terminate
// command to whichever node owns each VM (per-instance ec2.cmd.<id>), mirroring
// the gateway TerminateInstances path. The owning daemon runs the full teardown
// — detach + force-delete the worker's primary ENI and clear its
// spinifex-vpc-enis record — so a dangling in-use ENI cannot pin the customer
// VPC/subnet undeletable. A local-only vmMgr.Get would skip any
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
		slog.Debug("TerminateWorkerInstances: no owner for instance, already gone", "instanceId", instanceID)
		return nil
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
