package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// SystemOwnerAlias is the fixed owner alias written to AMI config on promotion.
// It must be a non-account-ID string so callerCanReadAMI treats the AMI as a
// system image visible to all accounts.
const SystemOwnerAlias = "system"

// PromoteImageOpts is the input for PromoteSystemImage and the NATS message
// body for the spinifex.image.promote topic.
type PromoteImageOpts struct {
	ImageID string `json:"ImageID"`
}

// PromoteImageResult summarises what changed after a successful promotion and
// is also the NATS reply for the spinifex.image.promote topic.
type PromoteImageResult struct {
	// PreviousOwner is the ImageOwnerAlias before promotion (the account ID).
	PreviousOwner string `json:"PreviousOwner"`
}

// PromoteSystemImage promotes an account-owned AMI to a system image by
// rewriting its ImageOwnerAlias to SystemOwnerAlias and re-keying its snapshot
// document under the global account. After the call the AMI is immediately
// visible to all accounts via DescribeImages.
//
// Guards:
//   - ImageID must have "ami-" prefix
//   - config.json must exist and parse cleanly
//   - AMI must currently be account-owned; already-system AMIs are rejected
//   - the snapshot document must be readable, so it can be re-keyed
func PromoteSystemImage(store objectstore.ObjectStore, bucket string, opts PromoteImageOpts) (*PromoteImageResult, error) {
	if !strings.HasPrefix(opts.ImageID, "ami-") {
		return nil, errors.New(awserrors.ErrorInvalidAMIIDMalformed)
	}

	meta, err := readAMI(store, bucket, opts.ImageID)
	switch {
	case err == nil:
		// ok
	case objectstore.IsNoSuchKeyError(err):
		return nil, errors.New(awserrors.ErrorInvalidAMIIDNotFound)
	case errors.Is(err, ebsmetadata.ErrCorruptDocument):
		return nil, errors.New(awserrors.ErrorInvalidAMIIDNotFound)
	default:
		slog.Error("PromoteSystemImage: read AMI config", "imageId", opts.ImageID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	if meta.ImageOwnerAlias == "" || !utils.IsAccountID(meta.ImageOwnerAlias) {
		return nil, fmt.Errorf("%s is already a system-owned AMI (owner: %q); promotion not allowed", opts.ImageID, meta.ImageOwnerAlias)
	}

	prev := meta.ImageOwnerAlias
	metaStore := ebsmetadata.NewStore(store, bucket)

	// The snapshot document has to move before the alias is rewritten: readers
	// derive its account from the alias, so a system alias over a tenant-keyed
	// snapshot resolves to nothing.
	snap, moveSnapshot, err := readPromotedSnapshot(metaStore, prev, meta.SnapshotID)
	switch {
	case err == nil:
		// ok
	case errors.Is(err, ebsmetadata.ErrCorruptDocument):
		return nil, fmt.Errorf("%s references snapshot %s, whose document is corrupt and cannot be re-keyed; promotion not allowed",
			opts.ImageID, meta.SnapshotID)
	default:
		slog.Error("PromoteSystemImage: read snapshot document", "imageId", opts.ImageID, "snapshotId", meta.SnapshotID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if moveSnapshot {
		snap.OwnerID = utils.GlobalAccountID
		if err := metaStore.PutSnapshot(context.Background(), snap); err != nil {
			slog.Error("PromoteSystemImage: write snapshot document under the global account",
				"imageId", opts.ImageID, "snapshotId", meta.SnapshotID, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
	}

	meta.ImageOwnerAlias = SystemOwnerAlias
	if err := metaStore.PutAMI(context.Background(), meta); err != nil {
		slog.Error("PromoteSystemImage: write AMI document", "imageId", opts.ImageID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Last, so every intermediate state resolves: until the alias is rewritten
	// readers look under prev, and afterwards under the global account.
	if moveSnapshot {
		if err := metaStore.DeleteSnapshot(context.Background(), prev, meta.SnapshotID); err != nil {
			slog.Error("PromoteSystemImage: delete the superseded snapshot document; the promoting account can still delete its blocks",
				"imageId", opts.ImageID, "snapshotId", meta.SnapshotID, "previousOwner", prev, "err", err)
		}
	}

	slog.Info("PromoteSystemImage completed", "imageId", opts.ImageID, "previousOwner", prev,
		"newOwner", SystemOwnerAlias, "snapshotMoved", moveSnapshot)
	return &PromoteImageResult{PreviousOwner: prev}, nil
}

// readPromotedSnapshot reads the document an AMI's snapshot is keyed under
// before promotion. A bundled image has no snapshot of its own, which is not an
// error: it already resolves under the global account by falling back.
func readPromotedSnapshot(store *ebsmetadata.Store, owner, snapshotID string) (ebsmetadata.Snapshot, bool, error) {
	if snapshotID == "" {
		return ebsmetadata.Snapshot{}, false, nil
	}
	snap, err := store.GetSnapshot(context.Background(), owner, snapshotID)
	switch {
	case err == nil:
		return snap, true, nil
	case objectstore.IsNoSuchKeyError(err):
		return ebsmetadata.Snapshot{}, false, nil
	default:
		return ebsmetadata.Snapshot{}, false, err
	}
}

// GetAMIMetadata reads and returns the control-plane document for the given
// image ID. Returns ErrorInvalidAMIIDNotFound for missing or corrupt documents.
func GetAMIMetadata(store objectstore.ObjectStore, bucket, imageID string) (ebsmetadata.AMI, error) {
	meta, err := readAMI(store, bucket, imageID)
	switch {
	case err == nil:
		return meta, nil
	case objectstore.IsNoSuchKeyError(err):
		return ebsmetadata.AMI{}, errors.New(awserrors.ErrorInvalidAMIIDNotFound)
	case errors.Is(err, ebsmetadata.ErrCorruptDocument):
		return ebsmetadata.AMI{}, errors.New(awserrors.ErrorInvalidAMIIDNotFound)
	default:
		return ebsmetadata.AMI{}, errors.New(awserrors.ErrorServerInternal)
	}
}
