package daemon

import (
	"log/slog"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
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
	accountID := utils.AccountIDFromMsg(msg)
	input := &ec2.DisassociateIamInstanceProfileInput{}
	if errResp := utils.UnmarshalJsonPayload(input, msg.Data); errResp != nil {
		if err := msg.Respond(errResp); err != nil {
			slog.Error("handleIamProfileDisassociate: respond failed", "err", err)
		}
		return
	}

	result, err := d.instanceService.DisassociateIamProfileAssociation(input, accountID)
	if err != nil {
		respondWithError(msg, awserrors.ValidErrorCode(err.Error()))
		return
	}
	respondWithJSON(msg, result)
}

// handleIamProfileReplace services the replace fan-out. Same contract as
// handleIamProfileDisassociate: every daemon responds (JSON null on
// non-owners) so the gateway collector can exit early.
func (d *Daemon) handleIamProfileReplace(msg *nats.Msg) {
	accountID := utils.AccountIDFromMsg(msg)
	input := &ec2.ReplaceIamInstanceProfileAssociationInput{}
	if errResp := utils.UnmarshalJsonPayload(input, msg.Data); errResp != nil {
		if err := msg.Respond(errResp); err != nil {
			slog.Error("handleIamProfileReplace: respond failed", "err", err)
		}
		return
	}

	result, err := d.instanceService.ReplaceIamProfileAssociation(input, accountID)
	if err != nil {
		respondWithError(msg, awserrors.ValidErrorCode(err.Error()))
		return
	}
	respondWithJSON(msg, result)
}

// handleIamProfileDescribe services the describe fan-out. An empty slice is a
// valid response (no matches on this daemon) and counts toward the gateway's
// expectedNodes collector; the gateway concatenates per-daemon slices.
func (d *Daemon) handleIamProfileDescribe(msg *nats.Msg) {
	accountID := utils.AccountIDFromMsg(msg)
	input := &ec2.DescribeIamInstanceProfileAssociationsInput{}
	if errResp := utils.UnmarshalJsonPayload(input, msg.Data); errResp != nil {
		if err := msg.Respond(errResp); err != nil {
			slog.Error("handleIamProfileDescribe: respond failed", "err", err)
		}
		return
	}

	out, err := d.instanceService.DescribeIamProfileAssociations(input, accountID)
	if err != nil {
		respondWithError(msg, awserrors.ValidErrorCode(err.Error()))
		return
	}
	respondWithJSON(msg, out)
}
