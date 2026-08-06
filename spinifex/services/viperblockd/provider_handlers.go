package viperblockd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go/aws"
	awss3 "github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/viperblock/viperblock"
	vbs3 "github.com/mulgadc/viperblock/viperblock/backends/s3"
	"github.com/nats-io/nats.go"
)

// bytesPerGiB converts between the GiB units viperblock.VolumeMetadata
// persists and the byte units the ebsprovider wire contract uses.
const bytesPerGiB = 1024 * 1024 * 1024

// providerObjectStoreFactory builds the objectstore.ObjectStore DeleteVolume
// and DeleteSnapshot use to remove S3 object prefixes. Tests override this to
// inject objectstore.NewMemoryObjectStore(), keeping the unit tests free of
// any network dependency.
var providerObjectStoreFactory = func(cfg *Config) objectstore.ObjectStore {
	return objectstore.NewS3ObjectStoreFromConfig(admin.DialTarget(cfg.S3Host), cfg.Region, cfg.AccessKey, cfg.SecretKey)
}

// registerProviderSubjects subscribes the ebs.provider.v1.* handlers that
// serve the ebsprovider.EBSProvider NATS contract from this daemon, moving
// viperblock engine construction out of the EC2 control-plane handlers and
// into the storage daemon that owns BaseDir and the mounted-volume registry.
//
// PublishVolume/UnpublishVolume are intentionally NOT registered here: the
// mount/unmount logic they would front lives inline in launchService's
// ebs.mount / ebs.{node}.unmount closures (~300 lines each, nbdkit process
// management and NBD confirmation polling included), and extracting it into
// a function both handlers could share is too invasive to do safely as a
// single-line addition to viperblockd.go.
func registerProviderSubjects(cfg *Config, nc *nats.Conn) error {
	subs := []struct {
		subject string
		handler nats.MsgHandler
	}{
		{ebsprovider.CapabilitiesSubject, handleProviderCapabilities},
		{ebsprovider.CreateVolumeSubject, func(msg *nats.Msg) { handleCreateVolume(cfg, msg) }},
		{ebsprovider.GetVolumeSubject, func(msg *nats.Msg) { handleGetVolume(cfg, msg) }},
		{ebsprovider.ExpandVolumeSubject, func(msg *nats.Msg) { handleExpandVolume(cfg, msg) }},
		{ebsprovider.DeleteVolumeSubject, func(msg *nats.Msg) { handleDeleteVolume(cfg, msg) }},
		{ebsprovider.DeleteSnapshotSubject, func(msg *nats.Msg) { handleDeleteSnapshot(cfg, msg) }},
	}
	for _, s := range subs {
		if _, err := nc.QueueSubscribe(s.subject, "spinifex-workers", s.handler); err != nil {
			return fmt.Errorf("subscribe to %s: %w", s.subject, err)
		}
	}

	snapshotCreateWildcard := ebsprovider.SnapshotCreateSubjectPrefix + "*"
	if _, err := nc.QueueSubscribe(snapshotCreateWildcard, "spinifex-workers", func(msg *nats.Msg) {
		handleCreateSnapshot(cfg, nc, msg)
	}); err != nil {
		return fmt.Errorf("subscribe to %s: %w", snapshotCreateWildcard, err)
	}

	return nil
}

// badRequestError maps a JSON decode failure to the wire error shape.
func badRequestError(err error) *ebsprovider.ProviderError {
	return &ebsprovider.ProviderError{Code: ebsprovider.ErrorCodeInvalidArgument, Message: fmt.Sprintf("bad request: %v", err)}
}

func unsupportedVersionError(got uint16) *ebsprovider.ProviderError {
	return &ebsprovider.ProviderError{Code: ebsprovider.ErrorCodeUnsupportedVersion, Message: fmt.Sprintf("unsupported schema version %d, want %d", got, ebsprovider.SchemaVersion)}
}

func invalidArgumentError(format string, args ...any) *ebsprovider.ProviderError {
	return &ebsprovider.ProviderError{Code: ebsprovider.ErrorCodeInvalidArgument, Message: fmt.Sprintf(format, args...)}
}

func notFoundError(format string, args ...any) *ebsprovider.ProviderError {
	return &ebsprovider.ProviderError{Code: ebsprovider.ErrorCodeNotFound, Message: fmt.Sprintf(format, args...)}
}

func alreadyExistsError(format string, args ...any) *ebsprovider.ProviderError {
	return &ebsprovider.ProviderError{Code: ebsprovider.ErrorCodeAlreadyExists, Message: fmt.Sprintf(format, args...)}
}

func volumeInUseError(format string, args ...any) *ebsprovider.ProviderError {
	return &ebsprovider.ProviderError{Code: ebsprovider.ErrorCodeVolumeInUse, Message: fmt.Sprintf(format, args...)}
}

func internalError(format string, args ...any) *ebsprovider.ProviderError {
	return &ebsprovider.ProviderError{Code: ebsprovider.ErrorCodeInternal, Message: fmt.Sprintf(format, args...)}
}

// volumeHandle and snapshotHandle produce the opaque handle strings this
// provider returns, mirroring memory.go's "memory://..." convention so
// callers never need a provider-specific branch to interpret them.
func volumeHandle(volumeID string) string     { return "viperblock://volume/" + volumeID }
func snapshotHandle(snapshotID string) string { return "viperblock://snapshot/" + snapshotID }

// findMountedVolume returns volumeID's MountedVolume entry if this node has
// it mounted, so handlers can prefer the live engine over opening a second
// one on the same volume (the double-writer bug this decouple removes).
func findMountedVolume(cfg *Config, volumeID string) (MountedVolume, bool) {
	cfg.mu.Lock()
	defer cfg.mu.Unlock()
	for _, volume := range cfg.MountedVolumes {
		if volume.Name == volumeID {
			return volume, true
		}
	}
	return MountedVolume{}, false
}

func handleProviderCapabilities(msg *nats.Msg) {
	var req ebsprovider.GetCapabilitiesRequest
	if err := json.Unmarshal(msg.Data, &req); err != nil {
		slog.Error("ebs.provider.capabilities: bad request", "err", err)
		respondJSON(msg, ebsprovider.GetCapabilitiesResponse{Versioned: ebsprovider.NewVersioned(), Error: badRequestError(err)})
		return
	}
	if req.SchemaVersion != ebsprovider.SchemaVersion {
		slog.Error("ebs.provider.capabilities: unsupported schema version", "version", req.SchemaVersion)
		respondJSON(msg, ebsprovider.GetCapabilitiesResponse{Versioned: ebsprovider.NewVersioned(), Error: unsupportedVersionError(req.SchemaVersion)})
		return
	}
	respondJSON(msg, ebsprovider.GetCapabilitiesResponse{
		Versioned: ebsprovider.NewVersioned(),
		Capabilities: ebsprovider.Capabilities{
			// Clones read chunks straight out of the source volume's S3
			// prefix (copy-on-write via SourceVolumeName/BaseBlockMap), and
			// every snapshot freezes the live checkpoint rather than
			// depending on a guest-coordinated quiesce.
			CopyOnWriteClone:        true,
			CrashConsistentSnapshot: true,
			// ExpandVolume refuses a volume that is mounted with a live VB
			// (see handleExpandVolume), and there is no API surfacing which
			// extents are sparse.
			OnlineExpansion:       false,
			SparseExtentReporting: false,
		},
	})
}

// buildProviderVBConfig assembles the viperblock.VB config CreateVolume hands
// to viperblock.New. Only storage-owned facts go into VolumeMetadata here:
// TenantID, Tags, VolumeType, IOPS, Throughput and AvailabilityZone are
// control-plane facts owned by ebsmetadata now, not viperblock's own state.
func buildProviderVBConfig(cfg *Config, volumeID string, volumeSizeBytes uint64, sourceSnapshotID, sourceVolumeID string) *viperblock.VB {
	vbconfig := &viperblock.VB{
		VolumeName: volumeID,
		VolumeSize: volumeSizeBytes,
		BaseDir:    cfg.BaseDir,
		Cache:      viperblock.Cache{Config: viperblock.CacheConfig{Size: 0}},
		VolumeConfig: viperblock.VolumeConfig{
			VolumeMetadata: viperblock.VolumeMetadata{
				VolumeID:  volumeID,
				SizeGiB:   volumeSizeBytes / bytesPerGiB,
				State:     "available",
				CreatedAt: time.Now().UTC(),
			},
		},
		MasterKey:         cfg.masterKey,
		EncryptionEnabled: cfg.masterKey != nil,
		GCEnabled:         cfg.GCEnabled,
	}
	if sourceSnapshotID != "" {
		vbconfig.SnapshotID = sourceSnapshotID
		vbconfig.SourceVolumeName = sourceVolumeID
	}
	return vbconfig
}

func handleCreateVolume(cfg *Config, msg *nats.Msg) {
	var req ebsprovider.CreateVolumeRequest
	if err := json.Unmarshal(msg.Data, &req); err != nil {
		slog.Error("ebs.provider.volume.create: bad request", "err", err)
		respondJSON(msg, ebsprovider.CreateVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: badRequestError(err)})
		return
	}
	if req.SchemaVersion != ebsprovider.SchemaVersion {
		slog.Error("ebs.provider.volume.create: unsupported schema version", "version", req.SchemaVersion)
		respondJSON(msg, ebsprovider.CreateVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: unsupportedVersionError(req.SchemaVersion)})
		return
	}
	if !validVolumeName(req.VolumeID) {
		slog.Error("ebs.provider.volume.create: invalid volume id", "volume", req.VolumeID)
		respondJSON(msg, ebsprovider.CreateVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: invalidArgumentError("invalid volume id %q", req.VolumeID)})
		return
	}
	if req.CapacityRange.RequiredBytes <= 0 || (req.CapacityRange.LimitBytes > 0 && req.CapacityRange.RequiredBytes > req.CapacityRange.LimitBytes) {
		respondJSON(msg, ebsprovider.CreateVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: invalidArgumentError("invalid capacity range")})
		return
	}
	if req.SourceSnapshotID != "" && req.SourceVolumeID == "" {
		respondJSON(msg, ebsprovider.CreateVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: invalidArgumentError("source_volume_id is required with source_snapshot_id")})
		return
	}

	existing, err := describeVolumeEngine(cfg, req.VolumeID)
	switch {
	case err == nil:
		if existing.CapacityBytes != req.CapacityRange.RequiredBytes {
			slog.Error("ebs.provider.volume.create: volume exists with different capacity", "volume", req.VolumeID)
			respondJSON(msg, ebsprovider.CreateVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: alreadyExistsError("volume %s already exists with a different capacity", req.VolumeID)})
			return
		}
		respondJSON(msg, ebsprovider.CreateVolumeResponse{Versioned: ebsprovider.NewVersioned(), Volume: existing})
		return
	case errors.Is(err, viperblock.ErrStateNotFound):
		// Volume does not exist yet: fall through and create it.
	default:
		slog.Error("ebs.provider.volume.create: existence check failed", "volume", req.VolumeID, "err", err)
		respondJSON(msg, ebsprovider.CreateVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: internalError("check existing volume: %v", err)})
		return
	}

	vbconfig := buildProviderVBConfig(cfg, req.VolumeID, utils.SafeInt64ToUint64(req.CapacityRange.RequiredBytes), req.SourceSnapshotID, req.SourceVolumeID)
	s3cfg := vbs3.S3Config{
		VolumeName: req.VolumeID,
		Bucket:     cfg.Bucket,
		Region:     cfg.Region,
		AccessKey:  cfg.AccessKey,
		SecretKey:  cfg.SecretKey,
		Host:       admin.DialTarget(cfg.S3Host),
	}
	vb, err := viperblock.New(vbconfig, "s3", s3cfg)
	if err != nil {
		slog.Error("ebs.provider.volume.create: new viperblock failed", "volume", req.VolumeID, "err", err)
		respondJSON(msg, ebsprovider.CreateVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: internalError("new viperblock: %v", err)})
		return
	}
	defer func() {
		vb.StopChunkUploader()
		vb.StopWALSyncer()
	}()
	vb.SetDebug(false)

	if err := vb.Backend.Init(); err != nil {
		slog.Error("ebs.provider.volume.create: backend init failed", "volume", req.VolumeID, "err", err)
		respondJSON(msg, ebsprovider.CreateVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: internalError("backend init: %v", err)})
		return
	}
	if err := vb.SaveState(); err != nil {
		slog.Error("ebs.provider.volume.create: save state failed", "volume", req.VolumeID, "err", err)
		respondJSON(msg, ebsprovider.CreateVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: internalError("save state: %v", err)})
		return
	}

	slog.Info("ebs.provider.volume.create: created", "volume", req.VolumeID, "capacityBytes", req.CapacityRange.RequiredBytes)
	respondJSON(msg, ebsprovider.CreateVolumeResponse{
		Versioned: ebsprovider.NewVersioned(),
		Volume: &ebsprovider.Volume{
			ID:            req.VolumeID,
			CapacityBytes: req.CapacityRange.RequiredBytes,
			State:         ebsprovider.VolumeStateAvailable,
			Handle:        volumeHandle(req.VolumeID),
		},
	})
}

// describeVolumeEngine resolves volumeID to a provider-neutral Volume,
// preferring a live mounted VB over opening a second engine on the same
// volume. The returned error satisfies errors.Is(err, viperblock.ErrStateNotFound)
// when the volume does not exist.
func describeVolumeEngine(cfg *Config, volumeID string) (*ebsprovider.Volume, error) {
	if mv, ok := findMountedVolume(cfg, volumeID); ok && mv.VB != nil {
		return &ebsprovider.Volume{
			ID:            volumeID,
			CapacityBytes: utils.SafeUint64ToInt64(mv.VB.GetVolumeSize()),
			State:         ebsprovider.VolumeStateInUse,
			Handle:        volumeHandle(volumeID),
		}, nil
	}

	vb, err := openVolumeVB(cfg, volumeID)
	if err != nil {
		return nil, err
	}
	defer func() {
		vb.StopChunkUploader()
		vb.StopWALSyncer()
	}()
	return &ebsprovider.Volume{
		ID:            volumeID,
		CapacityBytes: utils.SafeUint64ToInt64(vb.GetVolumeSize()),
		State:         ebsprovider.VolumeStateAvailable,
		Handle:        volumeHandle(volumeID),
	}, nil
}

func handleGetVolume(cfg *Config, msg *nats.Msg) {
	var req ebsprovider.GetVolumeRequest
	if err := json.Unmarshal(msg.Data, &req); err != nil {
		slog.Error("ebs.provider.volume.describe: bad request", "err", err)
		respondJSON(msg, ebsprovider.GetVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: badRequestError(err)})
		return
	}
	if req.SchemaVersion != ebsprovider.SchemaVersion {
		respondJSON(msg, ebsprovider.GetVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: unsupportedVersionError(req.SchemaVersion)})
		return
	}
	if !validVolumeName(req.VolumeID) {
		respondJSON(msg, ebsprovider.GetVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: invalidArgumentError("invalid volume id %q", req.VolumeID)})
		return
	}

	volume, err := describeVolumeEngine(cfg, req.VolumeID)
	if err != nil {
		if errors.Is(err, viperblock.ErrStateNotFound) {
			respondJSON(msg, ebsprovider.GetVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: notFoundError("volume %s not found", req.VolumeID)})
			return
		}
		slog.Error("ebs.provider.volume.describe: failed", "volume", req.VolumeID, "err", err)
		respondJSON(msg, ebsprovider.GetVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: internalError("describe volume: %v", err)})
		return
	}
	respondJSON(msg, ebsprovider.GetVolumeResponse{Versioned: ebsprovider.NewVersioned(), Volume: volume})
}

func handleExpandVolume(cfg *Config, msg *nats.Msg) {
	var req ebsprovider.ExpandVolumeRequest
	if err := json.Unmarshal(msg.Data, &req); err != nil {
		slog.Error("ebs.provider.volume.expand: bad request", "err", err)
		respondJSON(msg, ebsprovider.ExpandVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: badRequestError(err)})
		return
	}
	if req.SchemaVersion != ebsprovider.SchemaVersion {
		respondJSON(msg, ebsprovider.ExpandVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: unsupportedVersionError(req.SchemaVersion)})
		return
	}
	if !validVolumeName(req.VolumeID) {
		respondJSON(msg, ebsprovider.ExpandVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: invalidArgumentError("invalid volume id %q", req.VolumeID)})
		return
	}
	if req.CapacityRange.RequiredBytes <= 0 {
		respondJSON(msg, ebsprovider.ExpandVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: invalidArgumentError("invalid capacity range")})
		return
	}

	if mv, ok := findMountedVolume(cfg, req.VolumeID); ok && mv.VB != nil {
		slog.Error("ebs.provider.volume.expand: volume is mounted", "volume", req.VolumeID)
		respondJSON(msg, ebsprovider.ExpandVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: volumeInUseError("volume %s is mounted; detach before expanding", req.VolumeID)})
		return
	}

	vb, err := openVolumeVB(cfg, req.VolumeID)
	if err != nil {
		if errors.Is(err, viperblock.ErrStateNotFound) {
			respondJSON(msg, ebsprovider.ExpandVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: notFoundError("volume %s not found", req.VolumeID)})
			return
		}
		slog.Error("ebs.provider.volume.expand: open volume failed", "volume", req.VolumeID, "err", err)
		respondJSON(msg, ebsprovider.ExpandVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: internalError("open volume: %v", err)})
		return
	}
	defer func() {
		vb.StopChunkUploader()
		vb.StopWALSyncer()
	}()

	currentBytes := utils.SafeUint64ToInt64(vb.GetVolumeSize())
	if req.CapacityRange.RequiredBytes < currentBytes {
		respondJSON(msg, ebsprovider.ExpandVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: invalidArgumentError("volume expansion is grow-only")})
		return
	}

	vc := vb.VolumeConfig
	vc.VolumeMetadata.SizeGiB = utils.SafeInt64ToUint64(req.CapacityRange.RequiredBytes) / bytesPerGiB
	rawConfig, err := json.Marshal(vc)
	if err != nil {
		slog.Error("ebs.provider.volume.expand: marshal volume config failed", "volume", req.VolumeID, "err", err)
		respondJSON(msg, ebsprovider.ExpandVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: internalError("marshal volume config: %v", err)})
		return
	}
	if err := applyConfigUpdate(vb, types.EBSConfigUpdateRequest{Volume: req.VolumeID, VolumeConfig: rawConfig}); err != nil {
		slog.Error("ebs.provider.volume.expand: apply config update failed", "volume", req.VolumeID, "err", err)
		respondJSON(msg, ebsprovider.ExpandVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: internalError("apply config update: %v", err)})
		return
	}

	slog.Info("ebs.provider.volume.expand: expanded", "volume", req.VolumeID, "capacityBytes", req.CapacityRange.RequiredBytes)
	respondJSON(msg, ebsprovider.ExpandVolumeResponse{
		Versioned: ebsprovider.NewVersioned(),
		Volume: &ebsprovider.Volume{
			ID:            req.VolumeID,
			CapacityBytes: utils.SafeUint64ToInt64(vb.GetVolumeSize()),
			State:         ebsprovider.VolumeStateAvailable,
			Handle:        volumeHandle(req.VolumeID),
		},
	})
}

// stopMountedVolumeForDelete removes volumeID from cfg.MountedVolumes if this
// node has it mounted and stops its resources: config subscription,
// background goroutines, nbdkit process, socket file. This mirrors (rather
// than calls into) launchService's ebs.delete cleanup closure — extracting a
// shared helper would touch viperblockd.go beyond the single registration
// line this change is scoped to.
func stopMountedVolumeForDelete(cfg *Config, volumeID string) {
	cfg.mu.Lock()
	var matched MountedVolume
	matchIdx := -1
	for i, volume := range cfg.MountedVolumes {
		if volume.Name == volumeID {
			matched = volume
			matchIdx = i
			cfg.MountedVolumes = append(cfg.MountedVolumes[:i], cfg.MountedVolumes[i+1:]...)
			break
		}
	}
	cfg.mu.Unlock()

	if matchIdx < 0 {
		return
	}

	if matched.ConfigSub != nil {
		if err := matched.ConfigSub.Unsubscribe(); err != nil {
			slog.Error("ebs.provider.volume.delete: failed to unsubscribe config topic", "volume", volumeID, "err", err)
		}
	}
	if matched.VB != nil {
		matched.VB.StopChunkUploader()
		matched.VB.StopWALSyncer()
	}
	if err := utils.KillProcess(matched.PID); err != nil {
		slog.Error("ebs.provider.volume.delete: failed to kill nbdkit process", "pid", matched.PID, "err", err)
	}
	if matched.Socket != "" {
		if err := os.Remove(matched.Socket); err != nil && !os.IsNotExist(err) {
			slog.Error("ebs.provider.volume.delete: failed to delete nbd socket", "socket", matched.Socket, "err", err)
		}
	}
}

// deleteObjectPrefix deletes every object under prefix in bucket, paginating
// through ListObjectsV2. Mirrors handlers/ec2/volume's deleteS3Prefix so
// DeleteVolume and DeleteSnapshot get the same idempotent-when-absent
// behaviour without a second copy of the pagination loop.
func deleteObjectPrefix(ctx context.Context, store objectstore.ObjectStore, bucket, prefix string) error {
	var continuationToken *string
	for {
		listOutput, err := store.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{
			Bucket:            awssdk.String(bucket),
			Prefix:            awssdk.String(prefix),
			ContinuationToken: continuationToken,
		})
		if err != nil {
			return fmt.Errorf("list objects under %s: %w", prefix, err)
		}
		if len(listOutput.Contents) == 0 {
			break
		}
		for _, obj := range listOutput.Contents {
			if _, err := store.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: awssdk.String(bucket), Key: obj.Key}); err != nil {
				return fmt.Errorf("delete object %s: %w", awssdk.StringValue(obj.Key), err)
			}
		}
		if !awssdk.BoolValue(listOutput.IsTruncated) {
			break
		}
		continuationToken = listOutput.NextContinuationToken
	}
	return nil
}

func handleDeleteVolume(cfg *Config, msg *nats.Msg) {
	var req ebsprovider.DeleteVolumeRequest
	if err := json.Unmarshal(msg.Data, &req); err != nil {
		slog.Error("ebs.provider.volume.delete: bad request", "err", err)
		respondJSON(msg, ebsprovider.DeleteVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: badRequestError(err)})
		return
	}
	if req.SchemaVersion != ebsprovider.SchemaVersion {
		respondJSON(msg, ebsprovider.DeleteVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: unsupportedVersionError(req.SchemaVersion)})
		return
	}
	if !validVolumeName(req.VolumeID) {
		respondJSON(msg, ebsprovider.DeleteVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: invalidArgumentError("invalid volume id %q", req.VolumeID)})
		return
	}

	stopMountedVolumeForDelete(cfg, req.VolumeID)

	store := providerObjectStoreFactory(cfg)
	ctx := context.Background()
	if err := deleteObjectPrefix(ctx, store, cfg.Bucket, req.VolumeID+"-efi/"); err != nil {
		slog.Error("ebs.provider.volume.delete: failed to delete aux objects", "volume", req.VolumeID, "err", err)
		respondJSON(msg, ebsprovider.DeleteVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: internalError("delete aux objects: %v", err)})
		return
	}
	if err := deleteObjectPrefix(ctx, store, cfg.Bucket, req.VolumeID+"/"); err != nil {
		slog.Error("ebs.provider.volume.delete: failed to delete volume objects", "volume", req.VolumeID, "err", err)
		respondJSON(msg, ebsprovider.DeleteVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: internalError("delete volume objects: %v", err)})
		return
	}

	// Delete is permanent: remove any on-disk WAL/checkpoint cache left on
	// this node regardless of mount-tracking state, mirroring ebs.delete.
	if localPath, err := localVolumeDir(cfg.BaseDir, req.VolumeID); err == nil {
		if err := os.RemoveAll(localPath); err != nil {
			slog.Error("ebs.provider.volume.delete: failed to remove local volume directory", "volume", req.VolumeID, "path", localPath, "err", err)
		}
	}

	slog.Info("ebs.provider.volume.delete: deleted", "volume", req.VolumeID)
	respondJSON(msg, ebsprovider.DeleteVolumeResponse{Versioned: ebsprovider.NewVersioned()})
}

func handleDeleteSnapshot(cfg *Config, msg *nats.Msg) {
	var req ebsprovider.DeleteSnapshotRequest
	if err := json.Unmarshal(msg.Data, &req); err != nil {
		slog.Error("ebs.provider.snapshot.delete: bad request", "err", err)
		respondJSON(msg, ebsprovider.DeleteSnapshotResponse{Versioned: ebsprovider.NewVersioned(), Error: badRequestError(err)})
		return
	}
	if req.SchemaVersion != ebsprovider.SchemaVersion {
		respondJSON(msg, ebsprovider.DeleteSnapshotResponse{Versioned: ebsprovider.NewVersioned(), Error: unsupportedVersionError(req.SchemaVersion)})
		return
	}
	if !validVolumeName(req.SnapshotID) {
		respondJSON(msg, ebsprovider.DeleteSnapshotResponse{Versioned: ebsprovider.NewVersioned(), Error: invalidArgumentError("invalid snapshot id %q", req.SnapshotID)})
		return
	}

	store := providerObjectStoreFactory(cfg)
	if err := deleteObjectPrefix(context.Background(), store, cfg.Bucket, req.SnapshotID+"/"); err != nil {
		slog.Error("ebs.provider.snapshot.delete: failed", "snapshot", req.SnapshotID, "err", err)
		respondJSON(msg, ebsprovider.DeleteSnapshotResponse{Versioned: ebsprovider.NewVersioned(), Error: internalError("delete snapshot objects: %v", err)})
		return
	}

	slog.Info("ebs.provider.snapshot.delete: deleted", "snapshot", req.SnapshotID)
	respondJSON(msg, ebsprovider.DeleteSnapshotResponse{Versioned: ebsprovider.NewVersioned()})
}

func handleCreateSnapshot(cfg *Config, nc *nats.Conn, msg *nats.Msg) {
	var req ebsprovider.CreateSnapshotRequest
	if err := json.Unmarshal(msg.Data, &req); err != nil {
		slog.Error("ebs.provider.snapshot.create: bad request", "err", err)
		respondJSON(msg, ebsprovider.CreateSnapshotResponse{Versioned: ebsprovider.NewVersioned(), Error: badRequestError(err)})
		return
	}
	if req.SchemaVersion != ebsprovider.SchemaVersion {
		respondJSON(msg, ebsprovider.CreateSnapshotResponse{Versioned: ebsprovider.NewVersioned(), Error: unsupportedVersionError(req.SchemaVersion)})
		return
	}

	subjectVolumeID := strings.TrimPrefix(msg.Subject, ebsprovider.SnapshotCreateSubjectPrefix)
	if !validVolumeName(subjectVolumeID) || subjectVolumeID != req.VolumeID {
		slog.Error("ebs.provider.snapshot.create: subject/volume mismatch", "subject", msg.Subject, "volume", req.VolumeID)
		respondJSON(msg, ebsprovider.CreateSnapshotResponse{Versioned: ebsprovider.NewVersioned(), Error: invalidArgumentError("volume id %q in subject does not match request volume id %q", subjectVolumeID, req.VolumeID)})
		return
	}
	if !validVolumeName(req.SnapshotID) {
		respondJSON(msg, ebsprovider.CreateSnapshotResponse{Versioned: ebsprovider.NewVersioned(), Error: invalidArgumentError("invalid snapshot id %q", req.SnapshotID)})
		return
	}

	completionSubject, err := ebsprovider.SnapshotCompletionSubject(req.SnapshotID)
	if err != nil {
		respondJSON(msg, ebsprovider.CreateSnapshotResponse{Versioned: ebsprovider.NewVersioned(), Error: invalidArgumentError("%s", err.Error())})
		return
	}

	operationID := utils.GenerateResourceID("op")
	respondJSON(msg, ebsprovider.CreateSnapshotResponse{
		Versioned:         ebsprovider.NewVersioned(),
		OperationID:       operationID,
		CompletionSubject: completionSubject,
		Snapshot: &ebsprovider.Snapshot{
			ID:             req.SnapshotID,
			SourceVolumeID: req.VolumeID,
			State:          ebsprovider.SnapshotStatePending,
		},
	})

	// The caller is responsible for draining the volume (flushing guest
	// writes to the live checkpoint) before requesting a snapshot; that
	// coordination goes through the guest over ec2.cmd.<instanceID> and
	// stays a control-plane concern this provider boundary does not reach.
	go completeCreateSnapshot(cfg, nc, req, operationID, completionSubject)
}

// completeCreateSnapshot runs the snapshot work in the background and
// publishes the result to completionSubject, matching the accept-then-publish
// contract NATSProvider.CreateSnapshot waits on.
func completeCreateSnapshot(cfg *Config, nc *nats.Conn, req ebsprovider.CreateSnapshotRequest, operationID, completionSubject string) {
	completion := ebsprovider.CreateSnapshotResponse{
		Versioned:         ebsprovider.NewVersioned(),
		OperationID:       operationID,
		CompletionSubject: completionSubject,
	}

	snapshot, err := snapshotVolumeEngine(cfg, req.VolumeID, req.SnapshotID)
	if err != nil {
		slog.Error("ebs.provider.snapshot.create: snapshot failed", "volume", req.VolumeID, "snapshot", req.SnapshotID, "err", err)
		code := ebsprovider.ErrorCodeInternal
		if errors.Is(err, viperblock.ErrStateNotFound) {
			code = ebsprovider.ErrorCodeNotFound
		}
		completion.Error = &ebsprovider.ProviderError{Code: code, Message: err.Error()}
	} else {
		completion.Snapshot = snapshot
	}

	data, err := json.Marshal(completion)
	if err != nil {
		slog.Error("ebs.provider.snapshot.create: failed to marshal completion", "volume", req.VolumeID, "snapshot", req.SnapshotID, "err", err)
		return
	}
	if err := nc.Publish(completionSubject, data); err != nil {
		slog.Error("ebs.provider.snapshot.create: failed to publish completion", "subject", completionSubject, "err", err)
	}
}

// snapshotVolumeEngine creates snapshotID off volumeID's live checkpoint,
// preferring an already-mounted VB over opening a second engine on the same
// volume. Draining the volume so the checkpoint is current is the caller's
// responsibility (see handleCreateSnapshot).
func snapshotVolumeEngine(cfg *Config, volumeID, snapshotID string) (*ebsprovider.Snapshot, error) {
	if mv, ok := findMountedVolume(cfg, volumeID); ok && mv.VB != nil {
		if err := mv.VB.LoadLiveCheckpoint(); err != nil {
			return nil, fmt.Errorf("load live checkpoint: %w", err)
		}
		if _, err := mv.VB.CreateSnapshot(snapshotID); err != nil {
			return nil, fmt.Errorf("create snapshot: %w", err)
		}
		return &ebsprovider.Snapshot{
			ID:             snapshotID,
			SourceVolumeID: volumeID,
			SizeBytes:      utils.SafeUint64ToInt64(mv.VB.GetVolumeSize()),
			CreatedAt:      time.Now().UTC(),
			State:          ebsprovider.SnapshotStateCompleted,
			Handle:         snapshotHandle(snapshotID),
		}, nil
	}

	s3cfg := vbs3.S3Config{
		VolumeName: volumeID,
		Bucket:     cfg.Bucket,
		Region:     cfg.Region,
		AccessKey:  cfg.AccessKey,
		SecretKey:  cfg.SecretKey,
		Host:       admin.DialTarget(cfg.S3Host),
	}
	vbconfig := &viperblock.VB{
		VolumeName:        volumeID,
		VolumeSize:        1, // Recalculated on LoadState.
		BaseDir:           cfg.BaseDir,
		Cache:             viperblock.Cache{Config: viperblock.CacheConfig{Size: 0}},
		MasterKey:         cfg.masterKey,
		EncryptionEnabled: cfg.masterKey != nil,
		GCEnabled:         cfg.GCEnabled,
	}
	vb, err := viperblock.New(vbconfig, "s3", s3cfg)
	if err != nil {
		return nil, fmt.Errorf("new viperblock: %w", err)
	}
	defer func() {
		vb.StopChunkUploader()
		vb.StopWALSyncer()
	}()
	if err := vb.Backend.Init(); err != nil {
		return nil, fmt.Errorf("backend init: %w", err)
	}
	if err := loadStateWithRetry(vb, volumeID); err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}
	if err := vb.LoadLiveCheckpoint(); err != nil {
		return nil, fmt.Errorf("load live checkpoint: %w", err)
	}
	if _, err := vb.CreateSnapshot(snapshotID); err != nil {
		return nil, fmt.Errorf("create snapshot: %w", err)
	}
	return &ebsprovider.Snapshot{
		ID:             snapshotID,
		SourceVolumeID: volumeID,
		SizeBytes:      utils.SafeUint64ToInt64(vb.GetVolumeSize()),
		CreatedAt:      time.Now().UTC(),
		State:          ebsprovider.SnapshotStateCompleted,
		Handle:         snapshotHandle(snapshotID),
	}, nil
}
