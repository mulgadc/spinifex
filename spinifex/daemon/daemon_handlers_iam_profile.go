package daemon

import (
	"context"

	"github.com/mulgadc/spinifex/contracts/ec2/v1"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/nats-io/nats.go"
)

// handleAssociateIamInstanceProfile services the per-instance Associate path.
// Gateway pre-validates the profile reference and iam:PassRole; ownership is
// checked by checkInstanceOwnership before dispatch.
func (d *Daemon) handleAssociateIamInstanceProfile(ctx context.Context, msg *nats.Msg, command ec2v1.EC2InstanceCommand, instance *vm.VM) string {
	result, err := d.instanceService.AssociateIamInstanceProfile(ctx, instance, command)
	if err != nil {
		return respondServiceErrorOutcome(d.node, msg, err)
	}
	respondWithJSON(d.node, msg, result)
	return outcomeSuccess
}
