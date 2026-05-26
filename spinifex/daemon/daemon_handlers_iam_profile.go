package daemon

import (
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go"
)

// handleAssociateIamInstanceProfile services the per-instance Associate path
// dispatched from handleEC2Events. The gateway has already resolved the
// profile reference and enforced iam:PassRole; instance ownership has been
// verified by checkInstanceOwnership before this point.
func (d *Daemon) handleAssociateIamInstanceProfile(msg *nats.Msg, command types.EC2InstanceCommand, instance *vm.VM) {
	result, err := d.instanceService.AssociateIamInstanceProfile(instance, command)
	if err != nil {
		respondWithError(msg, awserrors.ValidErrorCode(err.Error()))
		return
	}
	respondWithJSON(msg, result)
}

// handleIamProfileDisassociate services the ec2.IamProfileAssociation.disassociate
// fan-out subject. Every daemon responds: the owner with Found=true after
// mutating vm.VM, non-owners (or daemons whose VM has a different account)
// with Found=false so the gateway's expectedNodes collector can exit before
// the timeout. Errors short-circuit the fan-out — only the owner can produce
// a meaningful error (e.g. persistence failure) since non-owners NoOp.
func (d *Daemon) handleIamProfileDisassociate(msg *nats.Msg) {
	accountID := utils.AccountIDFromMsg(msg)
	req := &types.IamProfileDisassociateRequest{}
	if errResp := utils.UnmarshalJsonPayload(req, msg.Data); errResp != nil {
		if err := msg.Respond(errResp); err != nil {
			slog.Error("handleIamProfileDisassociate: respond failed", "err", err)
		}
		return
	}

	result, err := d.instanceService.DisassociateIamProfileAssociation(*req, accountID)
	if err != nil {
		respondWithError(msg, awserrors.ValidErrorCode(err.Error()))
		return
	}
	respondWithJSON(msg, result)
}

// handleIamProfileReplace services the ec2.IamProfileAssociation.replace
// fan-out subject. Same response contract as handleIamProfileDisassociate:
// every daemon always responds (Found=false NoOp on non-owners) so the
// gateway collector exits early when the cluster is healthy.
func (d *Daemon) handleIamProfileReplace(msg *nats.Msg) {
	accountID := utils.AccountIDFromMsg(msg)
	req := &types.IamProfileReplaceRequest{}
	if errResp := utils.UnmarshalJsonPayload(req, msg.Data); errResp != nil {
		if err := msg.Respond(errResp); err != nil {
			slog.Error("handleIamProfileReplace: respond failed", "err", err)
		}
		return
	}

	result, err := d.instanceService.ReplaceIamProfileAssociation(*req, accountID)
	if err != nil {
		respondWithError(msg, awserrors.ValidErrorCode(err.Error()))
		return
	}
	respondWithJSON(msg, result)
}

// handleIamProfileDescribe services the ec2.IamProfileAssociation.describe
// fan-out subject. Empty Associations slice is a valid response (no matches
// on this daemon) and counts toward the gateway's expectedNodes early-exit
// collector. The gateway concatenates per-daemon slices.
func (d *Daemon) handleIamProfileDescribe(msg *nats.Msg) {
	accountID := utils.AccountIDFromMsg(msg)
	req := &types.IamProfileDescribeRequest{}
	if errResp := utils.UnmarshalJsonPayload(req, msg.Data); errResp != nil {
		if err := msg.Respond(errResp); err != nil {
			slog.Error("handleIamProfileDescribe: respond failed", "err", err)
		}
		return
	}

	respondWithJSON(msg, d.instanceService.DescribeIamProfileAssociations(*req, accountID))
}
