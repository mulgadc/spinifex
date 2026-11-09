package admin

import (
	"context"
	"errors"
	"fmt"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"log/slog"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
)

// RemoveImageOpts configures RemoveSystemImage.
type RemoveImageOpts struct {
	ImageID string
	// Force bypasses the dependency check, the ownership-shape check, and
	// the "AMI document missing/corrupt" check. Salvage-mode lever.
	Force bool
}

// RemoveImageResult names what RemoveSystemImage deleted. The provider
// deleted SnapshotID and then VolumeID; either may already have been absent.
type RemoveImageResult struct {
	VolumeID   string
	SnapshotID string
	// UEFIObjectsDeleted counts the image's UEFI variable store objects.
	UEFIObjectsDeleted int
}

// Dependents lists every resource that transitively backs an admin-imported
// AMI. Removing the AMI while any of these exist would corrupt the dependent.
type Dependents struct {
	Snapshots []string // EC2 snapshots whose VolumeID == the AMI ID (i.e. CopyImage-derived snaps)
	Volumes   []string // Volumes whose SnapshotID is snap-ami-<id> or a derived snap
	AMIs      []string // AMIs whose SnapshotID is a derived snap (CopyImage of a system AMI then re-copy)
}

// Empty is true when nothing depends on the AMI.
func (d Dependents) Empty() bool {
	return len(d.Snapshots) == 0 && len(d.Volumes) == 0 && len(d.AMIs) == 0
}

// VolumeStatus is what the provider answered about an image's volume.
type VolumeStatus int

const (
	// VolumePresent: GetVolume returned the volume and its capacity.
	VolumePresent VolumeStatus = iota
	// VolumeAbsent: GetVolume answered not_found.
	VolumeAbsent
	// VolumeUnknown: GetVolume failed any other way, as it does for a volume
	// whose persisted state is unreadable. The removal does not need it.
	VolumeUnknown
)

// RemovePreview captures AMI metadata, the provider's view of the image's
// volume, and dependents for the CLI confirmation prompt.
// PreviewRemoveSystemImage performs no deletions.
type RemovePreview struct {
	ImageID       string
	Name          string
	Owner         string
	Created       time.Time
	ConfigPresent bool // false when the AMI's ebsmetadata document is missing
	ConfigCorrupt bool // true when the document exists but is undecodable
	IsSystemOwned bool // ImageOwnerAlias is set and not an account ID

	// SnapshotID is the provider snapshot the removal deletes first.
	SnapshotID string
	// VolumeStatus says whether VolumeCapacityBytes is known; VolumeErr
	// holds the provider's error when the status is VolumeUnknown.
	VolumeStatus        VolumeStatus
	VolumeCapacityBytes int64
	VolumeErr           error

	Dependents Dependents
}

// SnapPrefix returns the provider snapshot ID backing an admin-imported AMI.
// ImportImage snapshots the volume as "snap-<volumeID>", and the volume ID IS
// the AMI ID.
func SnapPrefix(imageID string) string {
	return "snap-" + imageID
}

// PreviewRemoveSystemImage gathers AMI metadata, the image volume's capacity
// and dependents without mutating any state. Not-found, corrupt config,
// account-owned AMI and an unreadable volume are preview fields.
func PreviewRemoveSystemImage(ctx context.Context, store objectstore.ObjectStore, provider ebsprovider.EBSProvider, bucket, imageID string) (*RemovePreview, error) {
	if !strings.HasPrefix(imageID, "ami-") {
		return nil, errors.New(awserrors.ErrorInvalidAMIIDMalformed)
	}

	preview := &RemovePreview{ImageID: imageID, SnapshotID: SnapPrefix(imageID)}

	meta, configErr := readAMI(store, bucket, imageID)
	switch {
	case configErr == nil:
		preview.ConfigPresent = true
		preview.Name = meta.Name
		preview.Owner = meta.ImageOwnerAlias
		preview.Created = meta.CreationDate
		preview.IsSystemOwned = meta.ImageOwnerAlias != "" && !awsidentifiers.IsAccountID(meta.ImageOwnerAlias)
	case objectstore.IsNoSuchKeyError(configErr):
		// Config absent — salvage candidate. Leave ConfigPresent=false.
	case errors.Is(configErr, ebsmetadata.ErrCorruptDocument):
		preview.ConfigCorrupt = true
	default:
		return nil, fmt.Errorf("preview: read AMI config: %w", configErr)
	}

	// A GetVolume failure never aborts the preview: an unreadable volume is
	// what --force salvages, and the deletes need no readable state.
	volume, err := provider.GetVolume(ctx, ebsprovider.GetVolumeRequest{
		Versioned: ebsprovider.NewVersioned(),
		VolumeID:  imageID,
	})
	switch {
	case err == nil:
		preview.VolumeStatus = VolumePresent
		preview.VolumeCapacityBytes = volume.CapacityBytes
	case errors.Is(err, ebsprovider.ErrNotFound):
		preview.VolumeStatus = VolumeAbsent
	default:
		preview.VolumeStatus = VolumeUnknown
		preview.VolumeErr = err
	}

	deps, err := FindAMIDependents(store, bucket, imageID)
	if err != nil {
		return nil, fmt.Errorf("preview: find dependents: %w", err)
	}
	preview.Dependents = deps

	return preview, nil
}

// FindAMIDependents returns snapshots, volumes, and AMIs that depend on the
// given system AMI. Walk terminates one hop deep.
func FindAMIDependents(store objectstore.ObjectStore, bucket, imageID string) (Dependents, error) {
	var deps Dependents

	// Pass 1: collect derived snaps (CopyImage of this AMI writes a snap whose
	// VolumeID points back at the source AMI ID). Whole-cluster: a copy made by
	// any account still pins this AMI's blocks. Strict: this answer gates
	// deleting those blocks, and a document that failed to read is not evidence
	// that no copy depends on them.
	snapDocs, err := ebsmetadata.NewStore(store, bucket).ListAllSnapshotsStrict(context.Background())
	if err != nil {
		return Dependents{}, fmt.Errorf("list ebsmetadata snapshots: %w", err)
	}
	derived := map[string]bool{}
	for _, cfg := range snapDocs {
		// Skip the viperblock-internal snap for this AMI — it has no document
		// (it was written by viperblock.CreateSnapshot, not the EC2 service).
		if cfg.SnapshotID == SnapPrefix(imageID) {
			continue
		}
		if cfg.VolumeID == imageID {
			derived[cfg.SnapshotID] = true
			deps.Snapshots = append(deps.Snapshots, cfg.SnapshotID)
		}
	}

	// The set of snap IDs that a dependent volume might reference: the
	// admin-import's internal snap plus every CopyImage-derived snap.
	volSnapRefs := map[string]bool{SnapPrefix(imageID): true}
	for s := range derived {
		volSnapRefs[s] = true
	}

	// Pass 2: volumes whose SnapshotID matches. The ebsmetadata documents are
	// the whole fleet — a volume with blocks but no document does not exist as
	// far as the control plane is concerned — and they carry SnapshotID
	// already, so no second read per volume is needed. Strict for the same
	// reason as pass 1.
	volDocs, err := ebsmetadata.NewStore(store, bucket).ListAllVolumesStrict(context.Background())
	if err != nil {
		return Dependents{}, fmt.Errorf("list ebsmetadata volumes: %w", err)
	}
	for _, v := range volDocs {
		if volSnapRefs[v.SnapshotID] {
			deps.Volumes = append(deps.Volumes, v.VolumeID)
		}
	}

	// Pass 3: AMIs whose SnapshotID is a derived snap, skipping the target AMI
	// itself.
	docAMIs, err := ebsmetadata.NewStore(store, bucket).ListAMIs(context.Background())
	if err != nil {
		return Dependents{}, fmt.Errorf("list ebsmetadata AMIs: %w", err)
	}
	for _, a := range docAMIs {
		if a.ImageID == imageID {
			continue
		}
		if derived[a.SnapshotID] {
			deps.AMIs = append(deps.AMIs, a.ImageID)
		}
	}

	return deps, nil
}

// RemoveSystemImage deletes an admin-imported AMI after re-validating that it
// is system-owned and has no dependents (bypassed by --force). Its UEFI store
// and then its document go first, so the AMI vanishes before the provider
// deletes its snapshot and then its volume.
func RemoveSystemImage(ctx context.Context, store objectstore.ObjectStore, provider ebsprovider.EBSProvider, bucket string, opts RemoveImageOpts) (*RemoveImageResult, error) {
	if !strings.HasPrefix(opts.ImageID, "ami-") {
		return nil, errors.New(awserrors.ErrorInvalidAMIIDMalformed)
	}

	meta, configErr := readAMI(store, bucket, opts.ImageID)
	configMissing := objectstore.IsNoSuchKeyError(configErr)
	configCorrupt := errors.Is(configErr, ebsmetadata.ErrCorruptDocument)
	switch {
	case configErr == nil:
		// fine
	case configMissing, configCorrupt:
		if !opts.Force {
			return nil, errors.New(awserrors.ErrorInvalidAMIIDNotFound)
		}
	default:
		slog.Error("RemoveSystemImage: read AMI config", "imageId", opts.ImageID, "err", configErr)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	if configErr == nil && !opts.Force {
		if meta.ImageOwnerAlias != "" && awsidentifiers.IsAccountID(meta.ImageOwnerAlias) {
			return nil, fmt.Errorf("%s is account-owned (%s); use `aws ec2 deregister-image` followed by `aws ec2 delete-snapshot`",
				opts.ImageID, meta.ImageOwnerAlias)
		}
	}

	if !opts.Force {
		deps, err := FindAMIDependents(store, bucket, opts.ImageID)
		if err != nil {
			slog.Error("RemoveSystemImage: dependency walk", "imageId", opts.ImageID, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		if !deps.Empty() {
			return nil, &DependentError{ImageID: opts.ImageID, Dependents: deps}
		}
	}

	result := &RemoveImageResult{}

	// Step 0: drop the image's UEFI variable store before the document, as
	// DeregisterImage does, so a failure here leaves the image for a rerun.
	n, err := uefistore.DeleteObjects(ctx, store, bucket, opts.ImageID)
	result.UEFIObjectsDeleted = n
	if err != nil {
		return nil, fmt.Errorf("delete UEFI variable store: %w", err)
	}

	// Step 1: drop the ebsmetadata document first — the barrier that hides the
	// AMI from DescribeImages, so no new launch can land on blocks steps 2
	// and 3 are about to delete. Fatal, unlike the blocks: an AMI still advertised after
	// its blocks are gone is worse than one whose blocks outlive it.
	// An already-absent document is the barrier being satisfied, not a failure:
	// this is exactly the state of an orphaned image, whose blocks are the only
	// thing left to reclaim and the whole reason --force exists.
	if err := ebsmetadata.NewStore(store, bucket).DeleteAMI(ctx, opts.ImageID); err != nil && !objectstore.IsNoSuchKeyError(err) {
		return nil, fmt.Errorf("delete ebsmetadata document: %w", err)
	}

	// Step 2: the snapshot before the volume, because a provider snapshot may
	// refer to its volume's blocks; a failure here leaves both for a rerun.
	snapshotID := SnapPrefix(opts.ImageID)
	if err := provider.DeleteSnapshot(ctx, ebsprovider.DeleteSnapshotRequest{
		Versioned:  ebsprovider.NewVersioned(),
		SnapshotID: snapshotID,
	}); err != nil {
		return nil, fmt.Errorf("delete snapshot %s: %w", snapshotID, err)
	}
	result.SnapshotID = snapshotID

	// Step 3: the image's volume, which shares its ID. The provider refuses
	// a volume that is still published.
	if err := provider.DeleteVolume(ctx, ebsprovider.DeleteVolumeRequest{
		Versioned: ebsprovider.NewVersioned(),
		VolumeID:  opts.ImageID,
	}); err != nil {
		return nil, fmt.Errorf("delete volume %s: %w", opts.ImageID, err)
	}
	result.VolumeID = opts.ImageID

	slog.Info("RemoveSystemImage completed",
		"imageId", opts.ImageID,
		"snapshotId", result.SnapshotID,
		"uefiObjectsDeleted", result.UEFIObjectsDeleted,
		"force", opts.Force,
	)
	return result, nil
}

// DependentError is returned by RemoveSystemImage when dependent resources
// block deletion. The CLI prints the dependents list and exits non-zero.
type DependentError struct {
	ImageID    string
	Dependents Dependents
}

func (e *DependentError) Error() string {
	return fmt.Sprintf("refusing to remove %s: %d volumes, %d snapshots, %d AMIs depend on this image",
		e.ImageID, len(e.Dependents.Volumes), len(e.Dependents.Snapshots), len(e.Dependents.AMIs))
}

// readAMI resolves an AMI's control-plane document. It exists so the admin
// tooling can read an AMI without an ImageServiceImpl, which carries NATS.
func readAMI(store objectstore.ObjectStore, bucket, imageID string) (ebsmetadata.AMI, error) {
	return ebsmetadata.NewStore(store, bucket).GetAMI(context.Background(), imageID)
}
