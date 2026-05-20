package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_image "github.com/mulgadc/spinifex/spinifex/handlers/ec2/image"
	handlers_ec2_snapshot "github.com/mulgadc/spinifex/spinifex/handlers/ec2/snapshot"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/viperblock/viperblock"
)

// RemoveImageOpts configures RemoveSystemImage.
type RemoveImageOpts struct {
	ImageID string
	// Force bypasses the dependency check, the ownership-shape check, and
	// the "config.json missing/corrupt" check. Salvage-mode lever.
	Force bool
}

// RemoveImageResult summarises what was deleted from object storage.
type RemoveImageResult struct {
	ObjectsDeleted int
	BytesFreed     int64
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

// RemovePreview captures everything the CLI needs to render the confirmation
// prompt and decide whether to proceed: AMI metadata (or zero values when the
// config is missing/corrupt), backing-storage byte counts, and the dependent
// resource set. PreviewRemoveSystemImage performs no deletions.
type RemovePreview struct {
	ImageID       string
	Name          string
	Owner         string
	Created       time.Time
	ConfigPresent bool // false when ami-<id>/config.json is missing
	ConfigCorrupt bool // true when config.json exists but is undecodable
	IsSystemOwned bool // ImageOwnerAlias is set and not an account ID

	AMIObjectCount  int
	AMIBytesTotal   int64
	SnapObjectCount int
	SnapBytesTotal  int64

	Dependents Dependents
}

// SnapPrefix returns the viperblock-internal snapshot prefix backing an
// admin-imported AMI. v_utils.ImportDiskImage writes the snapshot as
// "snap-<volumeName>" where the volume name IS the AMI ID.
func SnapPrefix(imageID string) string {
	return "snap-" + imageID
}

// PreviewRemoveSystemImage walks the object store and gathers everything the
// caller needs to decide whether to remove an admin-imported AMI. It performs
// no mutations. Returned errors indicate unexpected I/O failures only —
// AMI-not-found, corrupt config, and account-owned AMIs are reflected in the
// preview fields, not as errors.
func PreviewRemoveSystemImage(store objectstore.ObjectStore, bucket, imageID string) (*RemovePreview, error) {
	if !strings.HasPrefix(imageID, "ami-") {
		return nil, errors.New(awserrors.ErrorInvalidAMIIDMalformed)
	}

	preview := &RemovePreview{ImageID: imageID}

	meta, configErr := readAMIConfig(store, bucket, imageID)
	switch {
	case configErr == nil:
		preview.ConfigPresent = true
		preview.Name = meta.Name
		preview.Owner = meta.ImageOwnerAlias
		preview.Created = meta.CreationDate
		preview.IsSystemOwned = meta.ImageOwnerAlias != "" && !utils.IsAccountID(meta.ImageOwnerAlias)
	case objectstore.IsNoSuchKeyError(configErr):
		// Config absent — salvage candidate. Leave ConfigPresent=false.
	case errors.Is(configErr, handlers_ec2_image.ErrCorruptAMIConfig):
		preview.ConfigCorrupt = true
	default:
		return nil, fmt.Errorf("preview: read AMI config: %w", configErr)
	}

	amiCount, amiBytes, err := sumPrefix(store, bucket, imageID+"/")
	if err != nil {
		return nil, fmt.Errorf("preview: sum ami prefix: %w", err)
	}
	preview.AMIObjectCount = amiCount
	preview.AMIBytesTotal = amiBytes

	snapCount, snapBytes, err := sumPrefix(store, bucket, SnapPrefix(imageID)+"/")
	if err != nil {
		return nil, fmt.Errorf("preview: sum snap prefix: %w", err)
	}
	preview.SnapObjectCount = snapCount
	preview.SnapBytesTotal = snapBytes

	deps, err := FindAMIDependents(store, bucket, imageID)
	if err != nil {
		return nil, fmt.Errorf("preview: find dependents: %w", err)
	}
	preview.Dependents = deps

	return preview, nil
}

// FindAMIDependents walks the bucket to find every resource that transitively
// depends on the given admin-imported AMI:
//
//  1. snap-*/metadata.json whose VolumeID == imageID  → derivedSnaps
//     (these are CopyImage-created snapshots that point back at the system AMI)
//  2. vol-*/config.json whose VolumeMetadata.SnapshotID ∈ {snap-ami-<id>} ∪ derivedSnaps
//  3. ami-*/config.json whose AMIMetadata.SnapshotID ∈ derivedSnaps
//
// The walk terminates one hop deep. CopyImage of an account AMI uses its own
// VolumeID, not the system AMI's, so further recursion is not required today.
func FindAMIDependents(store objectstore.ObjectStore, bucket, imageID string) (Dependents, error) {
	var deps Dependents

	prefixes, err := listCommonPrefixes(store, bucket)
	if err != nil {
		return Dependents{}, fmt.Errorf("list bucket prefixes: %w", err)
	}

	// Pass 1: collect derived snaps (CopyImage of this AMI writes a snap whose
	// VolumeID points back at the source AMI ID).
	derived := map[string]bool{}
	for _, p := range prefixes {
		if !strings.HasPrefix(p, "snap-") {
			continue
		}
		snapID := strings.TrimSuffix(p, "/")
		// Skip the viperblock-internal snap prefix for this AMI — it has no
		// metadata.json (it was written by viperblock.CreateSnapshot, not by
		// the EC2 snapshot service).
		if snapID == SnapPrefix(imageID) {
			continue
		}
		cfg, err := handlers_ec2_snapshot.ReadSnapshotConfig(store, bucket, snapID)
		if err != nil {
			if objectstore.IsNoSuchKeyError(err) || errors.Is(err, handlers_ec2_snapshot.ErrCorruptSnapshotMetadata) {
				continue
			}
			return Dependents{}, fmt.Errorf("read snapshot %s: %w", snapID, err)
		}
		if cfg.VolumeID == imageID {
			derived[snapID] = true
			deps.Snapshots = append(deps.Snapshots, snapID)
		}
	}

	// The set of snap IDs that a dependent volume might reference: the
	// admin-import's internal snap plus every CopyImage-derived snap.
	volSnapRefs := map[string]bool{SnapPrefix(imageID): true}
	for s := range derived {
		volSnapRefs[s] = true
	}

	// Pass 2: volumes (vol-*/config.json) whose SnapshotID matches.
	for _, p := range prefixes {
		if !strings.HasPrefix(p, "vol-") {
			continue
		}
		volID := strings.TrimSuffix(p, "/")
		cfg, err := readVolumeConfig(store, bucket, volID)
		if err != nil {
			if objectstore.IsNoSuchKeyError(err) {
				continue
			}
			// Skip corrupt volume configs rather than failing the whole walk —
			// they aren't safer to ignore but a corrupt vol-*/config.json
			// already means manual recovery is required for that volume.
			slog.Warn("FindAMIDependents: skipping unreadable volume config", "volumeId", volID, "err", err)
			continue
		}
		if volSnapRefs[cfg.VolumeMetadata.SnapshotID] {
			deps.Volumes = append(deps.Volumes, volID)
		}
	}

	// Pass 3: AMIs (ami-*/config.json) whose SnapshotID is a derived snap.
	// Skip the target AMI itself.
	for _, p := range prefixes {
		if !strings.HasPrefix(p, "ami-") {
			continue
		}
		otherAMI := strings.TrimSuffix(p, "/")
		if otherAMI == imageID {
			continue
		}
		meta, err := readAMIConfig(store, bucket, otherAMI)
		if err != nil {
			if objectstore.IsNoSuchKeyError(err) || errors.Is(err, handlers_ec2_image.ErrCorruptAMIConfig) {
				continue
			}
			slog.Warn("FindAMIDependents: skipping unreadable AMI config", "imageId", otherAMI, "err", err)
			continue
		}
		if derived[meta.SnapshotID] {
			deps.AMIs = append(deps.AMIs, otherAMI)
		}
	}

	return deps, nil
}

// RemoveSystemImage deletes an admin-imported AMI and its backing storage
// after re-validating the preconditions:
//
//  1. ami-<id>/config.json exists and parses (skipped under --force = salvage)
//  2. AMI is system-owned, not account-owned (--force bypasses)
//  3. No volumes / snapshots / AMIs depend on it (--force bypasses)
//
// Deletion order is intentional: the config.json is deleted FIRST so the AMI
// disappears from DescribeImages immediately, preventing new RunInstances
// from racing the block cleanup.
func RemoveSystemImage(store objectstore.ObjectStore, bucket string, opts RemoveImageOpts) (*RemoveImageResult, error) {
	if !strings.HasPrefix(opts.ImageID, "ami-") {
		return nil, errors.New(awserrors.ErrorInvalidAMIIDMalformed)
	}

	meta, configErr := readAMIConfig(store, bucket, opts.ImageID)
	configMissing := objectstore.IsNoSuchKeyError(configErr)
	configCorrupt := errors.Is(configErr, handlers_ec2_image.ErrCorruptAMIConfig)
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
		if meta.ImageOwnerAlias != "" && utils.IsAccountID(meta.ImageOwnerAlias) {
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

	// Step 1: drop config.json first — the barrier that hides the AMI from
	// DescribeImages so no new launches can land on the blocks we're deleting.
	if configErr == nil || (opts.Force && !configMissing) {
		n, b, err := deletePrefix(store, bucket, opts.ImageID+"/config.json")
		if err != nil {
			return nil, fmt.Errorf("delete config: %w", err)
		}
		result.ObjectsDeleted += n
		result.BytesFreed += b
	}

	// Step 2: drop the rest of ami-<id>/ (chunks, WAL, checkpoints).
	n, b, err := deletePrefix(store, bucket, opts.ImageID+"/")
	if err != nil {
		return nil, fmt.Errorf("delete ami prefix: %w", err)
	}
	result.ObjectsDeleted += n
	result.BytesFreed += b

	// Step 3: drop snap-<amiID>/ (the viperblock-internal snap checkpoint).
	n, b, err = deletePrefix(store, bucket, SnapPrefix(opts.ImageID)+"/")
	if err != nil {
		return nil, fmt.Errorf("delete snap prefix: %w", err)
	}
	result.ObjectsDeleted += n
	result.BytesFreed += b

	slog.Info("RemoveSystemImage completed",
		"imageId", opts.ImageID,
		"objectsDeleted", result.ObjectsDeleted,
		"bytesFreed", result.BytesFreed,
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

// readAMIConfig reads ami-<id>/config.json and returns the AMIMetadata.
// Mirrors ImageServiceImpl.GetAMIConfig but operates package-locally so the
// admin tooling doesn't require an ImageServiceImpl (which carries NATS).
func readAMIConfig(store objectstore.ObjectStore, bucket, imageID string) (viperblock.AMIMetadata, error) {
	key := imageID + "/config.json"
	res, err := store.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return viperblock.AMIMetadata{}, err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return viperblock.AMIMetadata{}, err
	}

	var state viperblock.VBState
	if err := json.Unmarshal(body, &state); err != nil {
		return viperblock.AMIMetadata{}, fmt.Errorf("%w: %s: %v", handlers_ec2_image.ErrCorruptAMIConfig, key, err)
	}
	return state.VolumeConfig.AMIMetadata, nil
}

// readVolumeConfig reads vol-<id>/config.json into VolumeConfig.
type volumeConfigWrapper struct {
	VolumeConfig viperblock.VolumeConfig `json:"VolumeConfig"`
}

func readVolumeConfig(store objectstore.ObjectStore, bucket, volumeID string) (*viperblock.VolumeConfig, error) {
	key := volumeID + "/config.json"
	res, err := store.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	var w volumeConfigWrapper
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, fmt.Errorf("decode %s: %v", key, err)
	}
	return &w.VolumeConfig, nil
}

// listCommonPrefixes returns the top-level "directory" prefixes in the bucket
// (e.g. "ami-xxx/", "vol-yyy/", "snap-zzz/"), exhausting any pagination.
func listCommonPrefixes(store objectstore.ObjectStore, bucket string) ([]string, error) {
	seen := map[string]bool{}
	var token *string
	for {
		out, err := store.ListObjectsV2(&s3.ListObjectsV2Input{
			Bucket:            aws.String(bucket),
			Delimiter:         aws.String("/"),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, err
		}
		for _, cp := range out.CommonPrefixes {
			if cp.Prefix == nil {
				continue
			}
			seen[*cp.Prefix] = true
		}
		if !aws.BoolValue(out.IsTruncated) {
			break
		}
		token = out.NextContinuationToken
	}
	prefixes := make([]string, 0, len(seen))
	for p := range seen {
		prefixes = append(prefixes, p)
	}
	return prefixes, nil
}

// sumPrefix returns (object count, total bytes) for every object under prefix.
// Used by the preview to surface what an operator is about to delete.
func sumPrefix(store objectstore.ObjectStore, bucket, prefix string) (int, int64, error) {
	var count int
	var bytes int64
	var token *string
	for {
		out, err := store.ListObjectsV2(&s3.ListObjectsV2Input{
			Bucket:            aws.String(bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return 0, 0, err
		}
		for _, obj := range out.Contents {
			count++
			if obj.Size != nil {
				bytes += *obj.Size
			}
		}
		if !aws.BoolValue(out.IsTruncated) {
			break
		}
		token = out.NextContinuationToken
	}
	return count, bytes, nil
}

// deletePrefix removes every object under prefix and returns count+bytes
// deleted. Used by RemoveSystemImage for both the single-key config.json
// barrier and the bulk ami-<id>/ and snap-<id>/ teardown.
func deletePrefix(store objectstore.ObjectStore, bucket, prefix string) (int, int64, error) {
	var count int
	var bytes int64
	var token *string
	for {
		out, err := store.ListObjectsV2(&s3.ListObjectsV2Input{
			Bucket:            aws.String(bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return count, bytes, err
		}
		for _, obj := range out.Contents {
			if obj.Key == nil {
				continue
			}
			if _, err := store.DeleteObject(&s3.DeleteObjectInput{
				Bucket: aws.String(bucket),
				Key:    obj.Key,
			}); err != nil {
				return count, bytes, fmt.Errorf("delete %s: %w", *obj.Key, err)
			}
			count++
			if obj.Size != nil {
				bytes += *obj.Size
			}
		}
		if !aws.BoolValue(out.IsTruncated) {
			break
		}
		token = out.NextContinuationToken
	}
	return count, bytes, nil
}
