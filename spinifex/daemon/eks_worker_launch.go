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

// RunWorkerInstance launches a nodegroup worker through the normal customer
// RunInstances path: customer-owned, primary ENI auto-created in input.SubnetId
// with input.SecurityGroupIds, no ManagedBy tag, no mgmt NIC — so the worker is
// visible in DescribeInstances and reclaimed by TerminateInstances like any
// other customer EC2. The EKS service hands us raw cloud-config YAML in
// input.UserData; RunInstances expects it base64-encoded, so wrap it here.
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

// TerminateWorkerInstances terminates each nodegroup worker via the same path
// the EC2 NATS terminate handler uses. A worker no longer tracked by the local
// vmMgr is treated as already-gone (idempotent success) so a retried
// DeleteNodegroup / scale-down does not wedge on instances that already drained.
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
