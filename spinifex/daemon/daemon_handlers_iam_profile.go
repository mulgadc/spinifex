package daemon

import (
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go"
)

// handleAssociateIamInstanceProfile services the per-instance Associate path.
// Gateway pre-validates the profile reference and iam:PassRole; ownership is
// checked by checkInstanceOwnership before dispatch.
func (d *Daemon) handleAssociateIamInstanceProfile(msg *nats.Msg, command types.EC2InstanceCommand, instance *vm.VM) {
	result, err := d.instanceService.AssociateIamInstanceProfile(instance, command)
	if err != nil {
		respondWithError(msg, awserrors.ValidErrorCode(err.Error()))
		return
	}
	respondWithJSON(msg, result)
}

// handleIamProfileDisassociate services the disassociate fan-out. The owning
// daemon responds with the mutated association; non-owners respond with JSON
// null so the gateway's expectedNodes collector can exit early.
func (d *Daemon) handleIamProfileDisassociate(msg *nats.Msg) {
	handleNATSRequest(msg, d.instanceService.DisassociateIamProfileAssociation)
}

// handleIamProfileReplace services the replace fan-out. Same contract as
// handleIamProfileDisassociate: every daemon responds (JSON null on
// non-owners) so the gateway collector can exit early.
func (d *Daemon) handleIamProfileReplace(msg *nats.Msg) {
	handleNATSRequest(msg, d.instanceService.ReplaceIamProfileAssociation)
}

// handleIamProfileDescribe services the describe fan-out. An empty slice is a
// valid response (no matches on this daemon) and counts toward the gateway's
// expectedNodes collector; the gateway concatenates per-daemon slices.
func (d *Daemon) handleIamProfileDescribe(msg *nats.Msg) {
	handleNATSRequest(msg, d.instanceService.DescribeIamProfileAssociations)
}
