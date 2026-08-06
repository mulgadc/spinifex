package admin

import (
	"context"
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
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata/vblegacy"
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
//
// BytesDeleted is the logical size of the deleted objects, not physical disk
// space: predastore reclaims a deleted object's bytes asynchronously via
// background compaction, so this count can be freed well after the call
// returns, or briefly not at all if compaction is stalled.
type RemoveImageResult struct {
	ObjectsDeleted int
	BytesDeleted   int64
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

// RemovePreview captures AMI metadata, byte counts, and dependents for the
// CLI confirmation prompt. PreviewRemoveSystemImage performs no deletions.
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

// PreviewRemoveSystemImage gathers AMI metadata and dependents without mutating
// any state. Not-found, corrupt config, and account-owned AMI are preview fields.
func PreviewRemoveSystemImage(store objectstore.ObjectStore, bucket, imageID string) (*RemovePreview, error) {
	if !strings.HasPrefix(imageID, "ami-") {
		return nil, errors.New(awserrors.ErrorInvalidAMIIDMalformed)
	}

	preview := &RemovePreview{ImageID: imageID}

	meta, _, configErr := readAMI(store, bucket, imageID)
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

// FindAMIDependents returns snapshots, volumes, and AMIs that depend on the
// given system AMI. Walk terminates one hop deep.
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

	// Pass 2: volumes whose SnapshotID matches, from the union of ebsmetadata
	// volume documents (provider-managed, no vol-<id>/config.json exists) and
	// legacy vol-<id>/config.json — same shape as pass 3's AMI union below.
	// Documents already carry SnapshotID, so no second read is needed for
	// them; only IDs found solely via the legacy prefix scan fall through to
	// readVolumeConfig, preserving the existing fail-closed-on-transport-error
	// behavior the deletion walk depends on.
	volDocs, err := ebsmetadata.NewStore(store, bucket).ListVolumes(context.Background())
	if err != nil {
		return Dependents{}, fmt.Errorf("list ebsmetadata volumes: %w", err)
	}
	documentedVols := make(map[string]bool, len(volDocs))
	for _, v := range volDocs {
		documentedVols[v.VolumeID] = true
		if volSnapRefs[v.SnapshotID] {
			deps.Volumes = append(deps.Volumes, v.VolumeID)
		}
	}
	legacyVolIDs, err := ebsmetadata.LegacyPrefixIDs(context.Background(), store, bucket, "vol-", "-efi", "-cloudinit")
	if err != nil {
		return Dependents{}, fmt.Errorf("list legacy volume prefixes: %w", err)
	}
	for _, volID := range legacyVolIDs {
		if documentedVols[volID] {
			continue
		}
		cfg, err := readVolumeConfig(store, bucket, volID)
		if err != nil {
			if objectstore.IsNoSuchKeyError(err) || errors.Is(err, errCorruptVolumeConfig) {
				continue
			}
			return Dependents{}, fmt.Errorf("read volume %s: %w", volID, err)
		}
		if volSnapRefs[cfg.VolumeMetadata.SnapshotID] {
			deps.Volumes = append(deps.Volumes, volID)
		}
	}

	// Pass 3: AMIs whose SnapshotID is a derived snap, skipping the target AMI
	// itself. Candidates are the union of ami-* prefixes (legacy/embedded) and
	// ebsmetadata AMI documents (provider path) — a provider-managed AMI may
	// have no ami-<id>/ prefix at all, so the prefix scan alone would miss it.
	amiIDs := map[string]bool{}
	for _, p := range prefixes {
		if strings.HasPrefix(p, "ami-") {
			amiIDs[strings.TrimSuffix(p, "/")] = true
		}
	}
	docAMIs, err := ebsmetadata.NewStore(store, bucket).ListAMIs(context.Background())
	if err != nil {
		return Dependents{}, fmt.Errorf("list ebsmetadata AMIs: %w", err)
	}
	for _, a := range docAMIs {
		amiIDs[a.ImageID] = true
	}

	for otherAMI := range amiIDs {
		if otherAMI == imageID {
			continue
		}
		meta, _, err := readAMI(store, bucket, otherAMI)
		if err != nil {
			if objectstore.IsNoSuchKeyError(err) || errors.Is(err, handlers_ec2_image.ErrCorruptAMIConfig) {
				continue
			}
			return Dependents{}, fmt.Errorf("read AMI %s: %w", otherAMI, err)
		}
		if derived[meta.SnapshotID] {
			deps.AMIs = append(deps.AMIs, otherAMI)
		}
	}

	return deps, nil
}

// RemoveSystemImage deletes an admin-imported AMI after re-validating that it
// is system-owned and has no dependents (bypassed by --force). config.json is
// deleted first so the AMI vanishes from DescribeImages before block cleanup.
func RemoveSystemImage(store objectstore.ObjectStore, bucket string, opts RemoveImageOpts) (*RemoveImageResult, error) {
	if !strings.HasPrefix(opts.ImageID, "ami-") {
		return nil, errors.New(awserrors.ErrorInvalidAMIIDMalformed)
	}

	meta, _, configErr := readAMI(store, bucket, opts.ImageID)
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
		result.BytesDeleted += b
	}

	// Step 2: drop the rest of ami-<id>/ (chunks, WAL, checkpoints).
	n, b, err := deletePrefix(store, bucket, opts.ImageID+"/")
	if err != nil {
		return nil, fmt.Errorf("delete ami prefix: %w", err)
	}
	result.ObjectsDeleted += n
	result.BytesDeleted += b

	// Step 3: drop snap-<amiID>/ (the viperblock-internal snap checkpoint).
	n, b, err = deletePrefix(store, bucket, SnapPrefix(opts.ImageID)+"/")
	if err != nil {
		return nil, fmt.Errorf("delete snap prefix: %w", err)
	}
	result.ObjectsDeleted += n
	result.BytesDeleted += b

	// Step 4: best-effort drop the ebsmetadata document too. The provider
	// path never writes config.json, so without this an ebsmetadata-only
	// AMI's document would survive steps 1-3 and keep DescribeImages
	// reporting an AMI whose blocks are already gone. Deleting a key that
	// was never written is a no-op, so this is safe for legacy-only AMIs.
	if err := ebsmetadata.NewStore(store, bucket).DeleteAMI(context.Background(), opts.ImageID); err != nil {
		slog.Warn("RemoveSystemImage: failed to delete ebsmetadata document", "imageId", opts.ImageID, "err", err)
	}

	slog.Info("RemoveSystemImage completed",
		"imageId", opts.ImageID,
		"objectsDeleted", result.ObjectsDeleted,
		"bytesDeleted", result.BytesDeleted,
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
	res, err := store.GetObject(context.Background(), &s3.GetObjectInput{
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
	if err := json.Unmarshal(viperblock.StateBody(body), &state); err != nil {
		return viperblock.AMIMetadata{}, fmt.Errorf("%w: %s: %w", handlers_ec2_image.ErrCorruptAMIConfig, key, err)
	}
	return state.VolumeConfig.AMIMetadata, nil
}

// readAMI resolves an AMI's metadata regardless of which EBS provider wrote
// it: an ebsmetadata document (the provider path's only representation)
// takes precedence over readAMIConfig's legacy config.json, which the
// provider path never writes. isDocument reports which representation was
// found, so callers know where a write-back or delete belongs.
func readAMI(store objectstore.ObjectStore, bucket, imageID string) (meta viperblock.AMIMetadata, isDocument bool, err error) {
	doc, docErr := ebsmetadata.NewStore(store, bucket).GetAMI(context.Background(), imageID)
	if docErr == nil {
		return vblegacy.AMIFromDocument(doc), true, nil
	}
	if !objectstore.IsNoSuchKeyError(docErr) {
		return viperblock.AMIMetadata{}, false, docErr
	}
	meta, err = readAMIConfig(store, bucket, imageID)
	return meta, false, err
}

// errCorruptVolumeConfig distinguishes an unparse-able config (walk continues)
// from a transport error (walk fails closed to prevent deleting live blocks).
var errCorruptVolumeConfig = errors.New("corrupt volume config")

// readVolumeConfig reads vol-<id>/config.json into VolumeConfig.
type volumeConfigWrapper struct {
	VolumeConfig viperblock.VolumeConfig `json:"VolumeConfig"`
}

func readVolumeConfig(store objectstore.ObjectStore, bucket, volumeID string) (*viperblock.VolumeConfig, error) {
	key := volumeID + "/config.json"
	res, err := store.GetObject(context.Background(), &s3.GetObjectInput{
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
	if err := json.Unmarshal(viperblock.StateBody(body), &w); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", errCorruptVolumeConfig, key, err)
	}
	return &w.VolumeConfig, nil
}

// listCommonPrefixes returns the top-level "directory" prefixes in the bucket
// (e.g. "ami-xxx/", "vol-yyy/", "snap-zzz/"), exhausting any pagination.
func listCommonPrefixes(store objectstore.ObjectStore, bucket string) ([]string, error) {
	seen := map[string]bool{}
	var token *string
	for {
		out, err := store.ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{
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
		out, err := store.ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{
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
		out, err := store.ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{
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
			if _, err := store.DeleteObject(context.Background(), &s3.DeleteObjectInput{
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
