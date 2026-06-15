package daemon

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	spxtypes "github.com/mulgadc/spinifex/spinifex/types"
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

// TerminateWorkerInstances terminates nodegroup workers. Already-gone instances
// are treated as success so retried DeleteNodegroup calls are idempotent.
func (d *Daemon) TerminateWorkerInstances(instanceIDs []string, accountID string) error {
	if d.instanceService == nil {
		return errors.New("eks worker: instance service not initialized")
	}
	var errs []error
	for _, id := range instanceIDs {
		if id == "" {
			continue
		}
		instance, ok := d.vmMgr.Get(id)
		if !ok {
			slog.Debug("TerminateWorkerInstances: instance already gone", "instanceId", id)
			continue
		}
		cmd := spxtypes.EC2InstanceCommand{
			ID:         id,
			Attributes: spxtypes.EC2CommandAttributes{TerminateInstance: true},
		}
		if err := d.instanceService.StopOrTerminateInstance(instance, cmd); err != nil {
			errs = append(errs, fmt.Errorf("terminate worker %s: %w", id, err))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}
