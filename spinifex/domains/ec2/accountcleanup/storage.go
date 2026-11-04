package accountcleanup

import (
	"context"
	"log/slog"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/accountinventory"
	ec2spotinstance "github.com/mulgadc/spinifex/spinifex/domains/ec2/spotinstance"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
)

// Storage kinds and the extra reasons the storage run reports.
const (
	KindVolume = "volume"
	KindTag    = "tag"

	// ReasonHolderPresent: the volume record shows an attachment, or a live
	// instance still maps the volume.
	ReasonHolderPresent = "holder-present"
	// ReasonVolumeStateUnproven: the volume is not in a state that proves it is
	// free, so it is kept.
	ReasonVolumeStateUnproven = "volume-state-unproven"
	// ReasonVolumeDeleting: the owner is reclaiming the volume; the record is
	// not gone yet.
	ReasonVolumeDeleting = "volume-deleting"

	// Incomplete reasons.
	ReasonVolumeInventoryIncomplete = "volume-inventory-incomplete"
	ReasonHolderProofIncomplete     = "holder-proof-incomplete"
	ReasonTagIndexUnread            = "tag-index-unread"
	ReasonTagOwnerUnresolved        = "tag-owner-unresolved"
)

// StorageRequest names the account whose volumes and tag residue are cleaned.
// Force is accepted and bypasses nothing: a holder, an unproven terminate or an
// incomplete inventory always keeps the volume.
type StorageRequest struct {
	AccountID      string `json:"account_id"`
	CorrelationRef string `json:"correlation_ref,omitempty"`
	Force          bool   `json:"force,omitempty"`
}

// StorageCleaner deletes an account's free volumes and the tag residue of
// instances, volumes and spot requests that no longer exist. It is
// destructive; the caller must hold an accepted account deletion.
type StorageCleaner interface {
	CleanupStorage(ctx context.Context, req *StorageRequest) (*StorageResult, error)
}

// VolumeLister is the complete-or-error volume inventory.
type VolumeLister interface {
	VolumeInventory(ctx context.Context, accountID string) ([]accountinventory.VolumeItem, error)
}

// VolumeDeleter sends the existing EC2 volume delete for one volume. It
// returns once the owner accepted the delete, not when the backing is reclaimed.
type VolumeDeleter interface {
	DeleteVolume(ctx context.Context, accountID, volumeID string) error
}

// SpotScanner is the spot-request owner's account scan.
type SpotScanner interface {
	ScanAccount(ctx context.Context, accountID string) (*ec2spotinstance.AccountScan, error)
}

var (
	_ VolumeLister   = (*accountinventory.Inventory)(nil)
	_ StorageCleaner = (*StorageCleanup)(nil)
)

// Resource-ID prefixes whose tag residue the storage run removes.
var storageTagPrefixes = []string{"i-", "vol-", "sir-"}

// StorageCleanup composes the storage owners.
type StorageCleanup struct {
	volumes   VolumeLister
	deleter   VolumeDeleter
	instances InstanceInventory
	teardown  TeardownReader
	spot      SpotScanner
	tags      TagIndexOwner
}

// NewStorage builds a storage cleanup. A nil dependency makes every run fail
// before it acts.
func NewStorage(volumes VolumeLister, deleter VolumeDeleter, instances InstanceInventory, teardown TeardownReader, spot SpotScanner, tags TagIndexOwner) *StorageCleanup {
	return &StorageCleanup{volumes: volumes, deleter: deleter, instances: instances, teardown: teardown, spot: spot, tags: tags}
}

// CleanupStorage deletes each volume that is available, shows no attachment and
// that no live instance maps, then removes tag-index entries whose owner record
// is gone. It never waits for reclaim: a volume the owner is deleting is
// reported pending and a rerun converges it. A read that does not complete
// deletes nothing.
func (c *StorageCleanup) CleanupStorage(ctx context.Context, req *StorageRequest) (*StorageResult, error) {
	if req == nil || req.AccountID == "" {
		return nil, awserrors.Errorf(awserrors.ErrorMissingParameter, "The request must specify an account ID.")
	}
	if !awsidentifiers.IsAccountID(req.AccountID) {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "The account ID must be 12 digits.")
	}
	if c == nil || c.volumes == nil || c.deleter == nil || c.instances == nil || c.teardown == nil || c.spot == nil || c.tags == nil {
		return nil, awserrors.Errorf(awserrors.ErrorServerInternal, "Storage cleanup is not fully wired.")
	}
	res := &StorageResult{AccountID: req.AccountID, CorrelationRef: req.CorrelationRef}
	defer res.finish()

	volumes, err := c.volumes.VolumeInventory(ctx, req.AccountID)
	if err != nil {
		slog.WarnContext(ctx, "CleanupStorage: volume inventory incomplete, nothing deleted", "account_id", req.AccountID, "err", err)
		res.incomplete(ReasonVolumeInventoryIncomplete)
		return res, nil
	}
	live, err := c.instances.AccountInstances(ctx, req.AccountID)
	if err != nil || !inventoryUsable(live) {
		slog.WarnContext(ctx, "CleanupStorage: no complete instance view, nothing deleted", "account_id", req.AccountID, "err", err)
		res.incomplete(ReasonHolderProofIncomplete)
		return res, nil
	}

	c.deleteFreeVolumes(ctx, req.AccountID, volumes, live, res)
	c.clearTagResidue(ctx, req.AccountID, live, res)
	return res, nil
}

// deleteFreeVolumes deletes the free volumes, re-reads the inventory and
// reports each volume from that re-read.
func (c *StorageCleanup) deleteFreeVolumes(ctx context.Context, accountID string, volumes []accountinventory.VolumeItem, live *ec2.DescribeInstancesOutput, res *StorageResult) {
	issued := map[string]bool{}
	blocked := map[string]string{}
	for _, v := range volumes {
		switch {
		case v.State == ec2.VolumeStateDeleting:
			// The owner is already reclaiming it; the re-read reports it.
		case v.AttachedInstance != "" || volumeMappedByLiveInstance(live, v.VolumeID):
			blocked[v.VolumeID] = ReasonHolderPresent
		case v.State != ec2.VolumeStateAvailable:
			blocked[v.VolumeID] = ReasonVolumeStateUnproven
		default:
			if err := c.deleter.DeleteVolume(ctx, accountID, v.VolumeID); err != nil {
				slog.WarnContext(ctx, "CleanupStorage: volume delete refused", "account_id", accountID, "volume_id", v.VolumeID, "err", err)
				if code, ok := awserrors.ResolveErrorCode(err); ok && code == awserrors.ErrorVolumeInUse {
					blocked[v.VolumeID] = ReasonHolderPresent
					continue
				}
				res.Items = append(res.Items, Item{ResourceID: v.VolumeID, Kind: KindVolume, State: v.State, Disposition: DispositionFailed, Reason: ReasonOwnerDeleteFailed})
				continue
			}
			issued[v.VolumeID] = true
		}
	}

	after, err := c.volumes.VolumeInventory(ctx, accountID)
	if err != nil {
		slog.WarnContext(ctx, "CleanupStorage: volume re-read failed", "account_id", accountID, "err", err)
		res.incomplete(ReasonVolumeInventoryIncomplete)
		for _, v := range volumes {
			if issued[v.VolumeID] || v.State == ec2.VolumeStateDeleting {
				res.Items = append(res.Items, Item{ResourceID: v.VolumeID, Kind: KindVolume, State: v.State, Disposition: DispositionPending, Reason: ReasonAbsenceUnproven})
			}
		}
	} else {
		present := map[string]accountinventory.VolumeItem{}
		for _, v := range after {
			present[v.VolumeID] = v
		}
		for _, v := range volumes {
			now, still := present[v.VolumeID]
			switch {
			case !issued[v.VolumeID] && v.State != ec2.VolumeStateDeleting:
				continue
			case !still:
				res.Items = append(res.Items, Item{ResourceID: v.VolumeID, Kind: KindVolume, Disposition: DispositionDeleted})
			case now.State == ec2.VolumeStateDeleting:
				res.Items = append(res.Items, Item{ResourceID: v.VolumeID, Kind: KindVolume, State: now.State, Disposition: DispositionPending, Reason: ReasonVolumeDeleting})
			default:
				res.Items = append(res.Items, Item{ResourceID: v.VolumeID, Kind: KindVolume, State: now.State, Disposition: DispositionFailed, Reason: ReasonStillPresent})
			}
		}
	}
	for id, reason := range blocked {
		res.Items = append(res.Items, Item{ResourceID: id, Kind: KindVolume, Disposition: DispositionBlocked, Reason: reason})
	}
}

// volumeMappedByLiveInstance reports whether a live instance's block device
// mappings name the volume. A volume record's own attachment fields can drift,
// so this is checked as well; terminated instances are not in the listing.
func volumeMappedByLiveInstance(live *ec2.DescribeInstancesOutput, volumeID string) bool {
	for _, reservation := range live.Reservations {
		for _, in := range reservation.Instances {
			if in.State != nil && aws.StringValue(in.State.Name) == ec2.InstanceStateNameTerminated {
				continue
			}
			for _, bdm := range in.BlockDeviceMappings {
				if bdm != nil && bdm.Ebs != nil && aws.StringValue(bdm.Ebs.VolumeId) == volumeID {
					return true
				}
			}
		}
	}
	return false
}

// clearTagResidue removes the tag entries of instances, volumes and spot
// requests whose owner re-read resolved and holds no record. An owner that did
// not resolve keeps its entries and the run incomplete.
func (c *StorageCleanup) clearTagResidue(ctx context.Context, accountID string, live *ec2.DescribeInstancesOutput, res *StorageResult) {
	tagged, err := c.tags.ScanAccountResourceIDs(ctx, accountID, storageTagPrefixes...)
	if err != nil {
		slog.WarnContext(ctx, "CleanupStorage: tag index read failed", "account_id", accountID, "err", err)
		res.incomplete(ReasonTagIndexUnread)
		return
	}
	owned, resolved := c.ownedResourceIDs(ctx, accountID, live)
	var cleared []string
	for _, id := range tagged {
		prefix := tagPrefix(id)
		switch {
		case !resolved[prefix]:
			res.incomplete(ReasonTagOwnerUnresolved)
			res.Items = append(res.Items, Item{ResourceID: id, Kind: KindTag, Disposition: DispositionResidue, Reason: ReasonOwnerRecordsUnresolved})
		case owned[id]:
			// The resource still exists; its tags are not residue.
		default:
			if err := c.tags.DeleteAllTags(ctx, accountID, id); err != nil {
				slog.WarnContext(ctx, "CleanupStorage: tag clear failed", "account_id", accountID, "resource_id", id, "err", err)
				res.incomplete(ReasonOwnerDeleteFailed)
				res.Items = append(res.Items, Item{ResourceID: id, Kind: KindTag, Disposition: DispositionFailed, Reason: ReasonOwnerDeleteFailed})
				continue
			}
			cleared = append(cleared, id)
		}
	}
	if len(cleared) == 0 {
		return
	}
	after, err := c.tags.ScanAccountResourceIDs(ctx, accountID, storageTagPrefixes...)
	if err != nil {
		res.incomplete(ReasonTagIndexUnread)
	}
	for _, id := range cleared {
		switch {
		case err != nil:
			res.Items = append(res.Items, Item{ResourceID: id, Kind: KindTag, Disposition: DispositionPending, Reason: ReasonAbsenceUnproven})
		case slices.Contains(after, id):
			res.Items = append(res.Items, Item{ResourceID: id, Kind: KindTag, Disposition: DispositionFailed, Reason: ReasonStillPresent})
		default:
			res.Items = append(res.Items, Item{ResourceID: id, Kind: KindTag, Disposition: DispositionDeleted})
		}
	}
}

// ownedResourceIDs reads, per prefix, the IDs whose owner still holds a record,
// and whether that read completed. Instances are owned while listed live or
// while their teardown is unfinished; a missing teardown record is not proof
// of completion, but tags of an instance with no live record are residue.
func (c *StorageCleanup) ownedResourceIDs(ctx context.Context, accountID string, live *ec2.DescribeInstancesOutput) (map[string]bool, map[string]bool) {
	owned := map[string]bool{}
	resolved := map[string]bool{}

	volumes, err := c.volumes.VolumeInventory(ctx, accountID)
	if err == nil {
		resolved["vol-"] = true
		for _, v := range volumes {
			owned[v.VolumeID] = true
		}
	}
	scan, err := c.spot.ScanAccount(ctx, accountID)
	if err == nil {
		resolved["sir-"] = true
		for _, r := range slices.Concat(scan.Active, scan.Terminal) {
			owned[r.RequestID] = true
		}
	}
	outstanding, err := c.teardown.AccountTeardownOutstanding(accountID)
	if err == nil {
		resolved["i-"] = true
		for _, o := range outstanding {
			owned[o.InstanceID] = true
		}
		for _, reservation := range live.Reservations {
			for _, in := range reservation.Instances {
				owned[aws.StringValue(in.InstanceId)] = true
			}
		}
	}
	return owned, resolved
}

func tagPrefix(id string) string {
	for _, prefix := range storageTagPrefixes {
		if strings.HasPrefix(id, prefix) {
			return prefix
		}
	}
	return ""
}
