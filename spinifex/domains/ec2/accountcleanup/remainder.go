package accountcleanup

import (
	"context"
	"log/slog"
	"slices"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
)

// ReasonRecordRemains: the owner still holds a record of the resource.
const ReasonRecordRemains = "record-remains"

// ComputeRemainder reads what compute the account still holds, changing nothing.
type ComputeRemainder interface {
	ComputeRemaining(ctx context.Context, accountID string) (*Outcome, error)
}

// StorageRemainder reads what volumes and tag residue the account still holds,
// changing nothing.
type StorageRemainder interface {
	StorageRemaining(ctx context.Context, accountID string) (*Outcome, error)
}

var (
	_ ComputeRemainder = (*ComputeCleanup)(nil)
	_ StorageRemainder = (*StorageCleanup)(nil)
)

func validRemainderAccount(accountID string) error {
	if accountID == "" {
		return awserrors.Errorf(awserrors.ErrorMissingParameter, "The request must specify an account ID.")
	}
	if !awsidentifiers.IsAccountID(accountID) {
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "The account ID must be 12 digits.")
	}
	return nil
}

// ComputeRemaining lists every instance the account still holds and every spot
// request that can still launch. A read that does not complete is an incomplete
// reason, never a shorter list. The result is a snapshot, not evidence of absence.
func (c *ComputeCleanup) ComputeRemaining(ctx context.Context, accountID string) (*Outcome, error) {
	if err := validRemainderAccount(accountID); err != nil {
		return nil, err
	}
	if c == nil || c.spot == nil || c.instances == nil || c.teardown == nil {
		return nil, awserrors.Errorf(awserrors.ErrorServerInternal, "Compute cleanup is not fully wired.")
	}
	res := &Outcome{AccountID: accountID}
	defer res.finish()

	scan, err := c.spot.ScanAccount(ctx, accountID)
	if err != nil {
		slog.WarnContext(ctx, "ComputeRemaining: spot scan failed", "account_id", accountID, "err", err)
		res.incomplete(ReasonSpotInventoryIncomplete)
	} else {
		for _, r := range slices.Concat(scan.Active, scan.Terminal) {
			if !terminalSpotState(r.State) {
				res.Items = append(res.Items, Item{ResourceID: r.RequestID, Kind: KindSpotRequest, State: r.State, Disposition: DispositionResidue, Reason: ReasonRecordRemains})
			}
		}
	}

	inventory, err := c.instances.AccountInstances(ctx, accountID)
	if err != nil || !inventoryUsable(inventory) {
		slog.WarnContext(ctx, "ComputeRemaining: instance inventory incomplete", "account_id", accountID, "err", err)
		res.incomplete(ReasonInstanceInventoryIncomplete)
	} else {
		for _, reservation := range inventory.Reservations {
			for _, in := range reservation.Instances {
				state := ""
				if in.State != nil {
					state = aws.StringValue(in.State.Name)
				}
				res.Items = append(res.Items, Item{ResourceID: aws.StringValue(in.InstanceId), Kind: KindInstance, State: state, Disposition: DispositionResidue, Reason: ReasonRecordRemains})
			}
		}
	}

	outstanding, err := c.teardown.AccountTeardownOutstanding(accountID)
	if err != nil {
		slog.WarnContext(ctx, "ComputeRemaining: teardown unread", "account_id", accountID, "err", err)
		res.incomplete(ReasonTeardownUnread)
	} else {
		for _, o := range outstanding {
			if !res.hasInstance(o.InstanceID) {
				res.Items = append(res.Items, Item{ResourceID: o.InstanceID, Kind: KindInstance, State: o.State, Disposition: DispositionResidue, Reason: ReasonTerminating})
			}
		}
	}
	return res, nil
}

// StorageRemaining lists every volume record the account still holds, deleting
// ones included, and the tag entries of resources whose owner holds no record.
func (c *StorageCleanup) StorageRemaining(ctx context.Context, accountID string) (*Outcome, error) {
	if err := validRemainderAccount(accountID); err != nil {
		return nil, err
	}
	if c == nil || c.volumes == nil || c.instances == nil || c.teardown == nil || c.spot == nil || c.tags == nil {
		return nil, awserrors.Errorf(awserrors.ErrorServerInternal, "Storage cleanup is not fully wired.")
	}
	res := &Outcome{AccountID: accountID}
	defer res.finish()

	volumes, err := c.volumes.VolumeInventory(ctx, accountID)
	if err != nil {
		slog.WarnContext(ctx, "StorageRemaining: volume inventory incomplete", "account_id", accountID, "err", err)
		res.incomplete(ReasonVolumeInventoryIncomplete)
	}
	for _, v := range volumes {
		reason := ReasonRecordRemains
		if v.State == ec2.VolumeStateDeleting {
			reason = ReasonVolumeDeleting
		}
		res.Items = append(res.Items, Item{ResourceID: v.VolumeID, Kind: KindVolume, State: v.State, Disposition: DispositionResidue, Reason: reason})
	}

	live, err := c.instances.AccountInstances(ctx, accountID)
	if err != nil || !inventoryUsable(live) {
		slog.WarnContext(ctx, "StorageRemaining: no complete instance view", "account_id", accountID, "err", err)
		res.incomplete(ReasonHolderProofIncomplete)
		return res, nil
	}
	outstanding, err := c.teardown.AccountTeardownOutstanding(accountID)
	if err != nil {
		slog.WarnContext(ctx, "StorageRemaining: teardown unread", "account_id", accountID, "err", err)
		res.incomplete(ReasonHolderProofIncomplete)
		return res, nil
	}

	tagged, err := c.tags.ScanAccountResourceIDs(ctx, accountID, storageTagPrefixes...)
	if err != nil {
		slog.WarnContext(ctx, "StorageRemaining: tag index read failed", "account_id", accountID, "err", err)
		res.incomplete(ReasonTagIndexUnread)
		return res, nil
	}
	owned, resolved := c.ownedResourceIDs(ctx, accountID, live, outstanding)
	for _, id := range tagged {
		switch {
		case !resolved[tagPrefix(id)]:
			res.incomplete(ReasonTagOwnerUnresolved)
			res.Items = append(res.Items, Item{ResourceID: id, Kind: KindTag, Disposition: DispositionResidue, Reason: ReasonOwnerRecordsUnresolved})
		case owned[id]:
			// The resource itself is reported; its tags are not residue.
		default:
			res.Items = append(res.Items, Item{ResourceID: id, Kind: KindTag, Disposition: DispositionResidue, Reason: ReasonRecordRemains})
		}
	}
	return res, nil
}
