package daemon

import (
	"context"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go"
)

// handleAssociateIamInstanceProfile services the per-instance Associate path.
// Gateway pre-validates the profile reference and iam:PassRole; ownership is
// checked by checkInstanceOwnership before dispatch.
func (d *Daemon) handleAssociateIamInstanceProfile(ctx context.Context, msg *nats.Msg, command types.EC2InstanceCommand, instance *vm.VM) {
	result, err := d.instanceService.AssociateIamInstanceProfile(ctx, instance, command)
	if err != nil {
		respondWithError(msg, awserrors.ValidErrorCodeFromError(err))
		return
	}
	respondWithJSON(msg, result)
}
