package viperblockd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go/aws"
	awss3 "github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/nbd"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	vbtypes "github.com/mulgadc/viperblock/types"
	"github.com/mulgadc/viperblock/viperblock"
	vbs3 "github.com/mulgadc/viperblock/viperblock/backends/s3"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
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
// PublishVolume/UnpublishVolume are node-addressed (ebsprovider.PublishSubject
// / UnpublishSubject already route to one node), so they are only registered
// when cfg.NodeName is set; there is no queue-group fallback the way the
// legacy ebs.mount/ebs.unmount subjects have.
func registerProviderSubjects(cfg *Config, nc *nats.Conn) error {
	subs := []struct {
		subject string
		handler nats.MsgHandler
	}{
		{ebsprovider.CapabilitiesSubject, handleProviderCapabilities},
		{ebsprovider.CreateVolumeSubject, func(msg *nats.Msg) { handleCreateVolume(cfg, msg) }},
		{ebsprovider.GetVolumeSubject, func(msg *nats.Msg) { handleGetVolume(cfg, msg) }},
		{ebsprovider.ExpandVolumeSubject, func(msg *nats.Msg) { handleExpandVolume(cfg, msg) }},
		{ebsprovider.DeleteVolumeSubject, func(msg *nats.Msg) { handleDeleteVolume(cfg, nc, msg) }},
		{ebsprovider.DeleteSnapshotSubject, func(msg *nats.Msg) { handleDeleteSnapshot(cfg, msg) }},
		{ebsprovider.CopySnapshotSubject, func(msg *nats.Msg) { handleCopySnapshot(cfg, msg) }},
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

	if cfg.NodeName == "" {
		slog.Warn("ebs.provider: NodeName is empty, this node cannot serve PublishVolume/UnpublishVolume attachments")
		return nil
	}

	publishSubject, err := ebsprovider.PublishSubject(cfg.NodeName)
	if err != nil {
		return fmt.Errorf("build publish subject: %w", err)
	}
	if _, err := nc.Subscribe(publishSubject, func(msg *nats.Msg) { handlePublishVolume(cfg, nc, msg) }); err != nil {
		return fmt.Errorf("subscribe to %s: %w", publishSubject, err)
	}

	unpublishSubject, err := ebsprovider.UnpublishSubject(cfg.NodeName)
	if err != nil {
		return fmt.Errorf("build unpublish subject: %w", err)
	}
	if _, err := nc.Subscribe(unpublishSubject, func(msg *nats.Msg) { handleUnpublishVolume(cfg, msg) }); err != nil {
		return fmt.Errorf("subscribe to %s: %w", unpublishSubject, err)
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

// errSnapshotDestinationExists lets handleCopySnapshot map a pre-existing
// destination to ErrorCodeAlreadyExists without string-matching
// viperblock.CopySnapshotMeta's error text, which carries no sentinel of
// its own.
var errSnapshotDestinationExists = errors.New("snapshot destination already exists")

// errSnapshotExistsElsewhere marks a snapshot ID already taken against a
// different volume. That is a conflicting recreate, not the idempotent repeat
// a matching source volume would make it.
var errSnapshotExistsElsewhere = errors.New("snapshot already exists on another volume")

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
			// CreateVolume can write a caller-supplied seed at offset 0 in the
			// engine it already builds, which is how an EFI variable store gets
			// its firmware VARS template without a control-plane engine.
			VolumeSeeding: true,
			// ExpandVolume refuses a volume that is mounted with a live VB
			// (see handleExpandVolume). The nbdkit Go binding exposes no
			// extents callback, so the export cannot report base:allocation.
			OnlineExpansion:       false,
			SparseExtentReporting: false,
			// A read-only publish starts nbdkit with -r, which sets the NBD
			// read-only transmission flag and makes the plugin refuse writes.
			ReadOnlyPublish: true,
			// mountVolume registers ebs.provider.v1.owner.{volumeID}.* for
			// every mounted volume (see subscribeOwnerSubjects).
			OwnerRouting: true,
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
	if req.SourceSnapshotID != "" && req.SourceSnapshotVolumeID == "" {
		respondJSON(msg, ebsprovider.CreateVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: invalidArgumentError("source_snapshot_volume_id is required with source_snapshot_id")})
		return
	}
	if err := ebsprovider.ValidateSeedData(req.SeedData); err != nil {
		slog.Error("ebs.provider.volume.create: seed data rejected", "volume", req.VolumeID, "err", err)
		respondJSON(msg, ebsprovider.CreateVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: invalidArgumentError("%v", err)})
		return
	}
	if int64(len(req.SeedData)) > req.CapacityRange.RequiredBytes {
		respondJSON(msg, ebsprovider.CreateVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: invalidArgumentError("seed data is larger than the requested capacity")})
		return
	}
	if len(req.SeedData) > 0 && req.SourceSnapshotID != "" {
		respondJSON(msg, ebsprovider.CreateVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: invalidArgumentError("seed data cannot be combined with a source snapshot")})
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

	vbconfig := buildProviderVBConfig(cfg, req.VolumeID, utils.SafeInt64ToUint64(req.CapacityRange.RequiredBytes), req.SourceSnapshotID, req.SourceSnapshotVolumeID)
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
	if req.SourceSnapshotID != "" {
		if perr := verifySourceSnapshot(vb, req.SourceSnapshotID, req.SourceSnapshotVolumeID); perr != nil {
			slog.Error("ebs.provider.volume.create: source snapshot rejected", "volume", req.VolumeID, "snapshot", req.SourceSnapshotID, "err", perr.Message)
			respondJSON(msg, ebsprovider.CreateVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: perr})
			return
		}
	}
	if len(req.SeedData) > 0 {
		if err := seedVolume(vb, req.SeedData); err != nil {
			slog.Error("ebs.provider.volume.create: seed failed", "volume", req.VolumeID, "bytes", len(req.SeedData), "err", err)
			respondJSON(msg, ebsprovider.CreateVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: internalError("seed volume: %v", err)})
			return
		}
	} else if err := vb.SaveState(); err != nil {
		slog.Error("ebs.provider.volume.create: save state failed", "volume", req.VolumeID, "err", err)
		respondJSON(msg, ebsprovider.CreateVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: internalError("save state: %v", err)})
		return
	}

	slog.Info("ebs.provider.volume.create: created", "volume", req.VolumeID, "capacityBytes", req.CapacityRange.RequiredBytes, "seedBytes", len(req.SeedData))
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

// verifySourceSnapshot reads the snapshot a new volume is being cloned from,
// before the volume is written. Without this a missing snapshot yields a
// blank volume and a wrong source volume is discovered only at mount.
// Snapshot metadata authenticates under its own recorded identity, so any VB
// sharing the backend and master key can read it.
func verifySourceSnapshot(vb *viperblock.VB, snapshotID, sourceVolumeID string) *ebsprovider.ProviderError {
	exists, err := snapshotConfigExists(vb, snapshotID)
	if err != nil {
		return internalError("probe source snapshot %s: %v", snapshotID, err)
	}
	if !exists {
		return notFoundError("source snapshot %s not found", snapshotID)
	}
	_, ident, err := vb.LoadSnapshotBlockMap(snapshotID)
	if err != nil {
		return internalError("read source snapshot %s: %v", snapshotID, err)
	}
	if ident.SourceVolumeName != sourceVolumeID {
		return invalidArgumentError("source snapshot %s was taken from volume %q, not %q", snapshotID, ident.SourceVolumeName, sourceVolumeID)
	}
	return nil
}

// seedVolume writes seed at offset 0 of a freshly created volume. Only
// CreateVolume calls it, and only when the existence check above found no
// volume, so it can never overwrite bytes a guest has already written.
func seedVolume(vb *viperblock.VB, seed []byte) error {
	var err error
	if vb.UseShardedWAL {
		err = vb.OpenShardedWAL()
	} else {
		err = vb.OpenWAL(&vb.WAL, fmt.Sprintf("%s/%s", vb.WAL.BaseDir, vbtypes.GetFilePath(vbtypes.FileTypeWALChunk, vb.WAL.WallNum.Load(), vb.GetVolume())))
	}
	if err != nil {
		return fmt.Errorf("open WAL: %w", err)
	}
	if err := vb.OpenWAL(&vb.BlockToObjectWAL, fmt.Sprintf("%s/%s", vb.WAL.BaseDir, vbtypes.GetFilePath(vbtypes.FileTypeWALBlock, vb.BlockToObjectWAL.WallNum.Load(), vb.GetVolume()))); err != nil {
		return fmt.Errorf("open block WAL: %w", err)
	}
	if err := vb.WriteAt(0, seed); err != nil {
		return fmt.Errorf("write seed: %w", err)
	}

	// Close is the durability boundary, not Flush: a partially persisted seed
	// leaves the caller believing bytes it never got are on the volume.
	if err := vb.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	if err := vb.RemoveLocalFiles(); err != nil {
		slog.Warn("ebs.provider.volume.create: could not remove local files after seeding", "volume", vb.GetVolume(), "err", err)
	}
	return nil
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

// ownerProbeTimeout bounds the wait for a volume's owner subject to answer.
// A mounted volume's owner is on the same NATS as the deleting node, so a
// reply is a round trip away; no reply within this means nobody holds it.
const ownerProbeTimeout = 2 * time.Second

// volumeMountedElsewhere asks volumeID's owner subject whether anyone answers
// for it. Only a node with the volume mounted subscribes that subject, so a
// reply means a live mount on some node, not necessarily this one.
func volumeMountedElsewhere(nc *nats.Conn, volumeID string) bool {
	if nc == nil {
		return false
	}
	subject, err := ebsprovider.GetVolumeOwnerSubject(volumeID)
	if err != nil {
		return false
	}
	payload, err := json.Marshal(ebsprovider.GetVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID})
	if err != nil {
		return false
	}
	if _, err := nc.Request(subject, payload, ownerProbeTimeout); err != nil {
		return false
	}
	return true
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

func handleDeleteVolume(cfg *Config, nc *nats.Conn, msg *nats.Msg) {
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

	// A published volume must outlive a delete: the nbdkit export and the
	// guest writing through it survive the metadata going away, so deleting
	// under them turns a refusable API call into later corruption.
	if _, mounted := findMountedVolume(cfg, req.VolumeID); mounted || volumeMountedElsewhere(nc, req.VolumeID) {
		slog.Error("ebs.provider.volume.delete: volume is published", "volume", req.VolumeID)
		respondJSON(msg, ebsprovider.DeleteVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: volumeInUseError("volume %s is published; unpublish it before deleting", req.VolumeID)})
		return
	}

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

// handleCopySnapshot serves ebs.provider.v1.snapshot.copy. Unlike
// handleCreateSnapshot, this is a plain synchronous request: duplicating a
// snapshot's metadata is a couple of small object writes, not a
// flush-drain-upload sequence slow enough to warrant the accept-then-publish
// pattern.
func handleCopySnapshot(cfg *Config, msg *nats.Msg) {
	var req ebsprovider.CopySnapshotRequest
	if err := json.Unmarshal(msg.Data, &req); err != nil {
		slog.Error("ebs.provider.snapshot.copy: bad request", "err", err)
		respondJSON(msg, ebsprovider.CopySnapshotResponse{Versioned: ebsprovider.NewVersioned(), Error: badRequestError(err)})
		return
	}
	if req.SchemaVersion != ebsprovider.SchemaVersion {
		respondJSON(msg, ebsprovider.CopySnapshotResponse{Versioned: ebsprovider.NewVersioned(), Error: unsupportedVersionError(req.SchemaVersion)})
		return
	}
	if !validVolumeName(req.SourceSnapshotID) || !validVolumeName(req.DestinationSnapshotID) || !validVolumeName(req.VolumeID) {
		respondJSON(msg, ebsprovider.CopySnapshotResponse{Versioned: ebsprovider.NewVersioned(), Error: invalidArgumentError("invalid source snapshot, destination snapshot, or volume id")})
		return
	}
	if req.SourceSnapshotID == req.DestinationSnapshotID {
		respondJSON(msg, ebsprovider.CopySnapshotResponse{Versioned: ebsprovider.NewVersioned(), Error: invalidArgumentError("destination snapshot %q must differ from the source", req.DestinationSnapshotID)})
		return
	}

	snapshot, err := copySnapshotOnVolume(cfg, req.VolumeID, req.SourceSnapshotID, req.DestinationSnapshotID)
	if err != nil {
		switch {
		case errors.Is(err, errSnapshotDestinationExists):
			respondJSON(msg, ebsprovider.CopySnapshotResponse{Versioned: ebsprovider.NewVersioned(), Error: alreadyExistsError("snapshot %s already exists", req.DestinationSnapshotID)})
		case errors.Is(err, viperblock.ErrSnapshotVolumeMismatch):
			respondJSON(msg, ebsprovider.CopySnapshotResponse{Versioned: ebsprovider.NewVersioned(), Error: invalidArgumentError("%v", err)})
		case errors.Is(err, viperblock.ErrStateNotFound), errors.Is(err, os.ErrNotExist):
			respondJSON(msg, ebsprovider.CopySnapshotResponse{Versioned: ebsprovider.NewVersioned(), Error: notFoundError("source snapshot %s not found on volume %s", req.SourceSnapshotID, req.VolumeID)})
		default:
			slog.Error("ebs.provider.snapshot.copy: failed", "volume", req.VolumeID, "source", req.SourceSnapshotID, "destination", req.DestinationSnapshotID, "err", err)
			respondJSON(msg, ebsprovider.CopySnapshotResponse{Versioned: ebsprovider.NewVersioned(), Error: internalError("copy snapshot: %v", err)})
		}
		return
	}

	slog.Info("ebs.provider.snapshot.copy: copied", "volume", req.VolumeID, "source", req.SourceSnapshotID, "destination", req.DestinationSnapshotID)
	respondJSON(msg, ebsprovider.CopySnapshotResponse{Versioned: ebsprovider.NewVersioned(), Snapshot: snapshot})
}

// snapshotConfigExists probes whether snapshotID's config object is already
// on the backend, the same check viperblock.CopySnapshotMeta makes
// internally before it writes. Doing it here first lets the handler surface
// a precise already_exists error instead of a generic internal one.
func snapshotConfigExists(vb *viperblock.VB, snapshotID string) (bool, error) {
	if _, err := vb.Backend.ReadFrom(snapshotID, vbtypes.FileTypeConfig, 0, 0, 0); err == nil {
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("probe destination snapshot %s: %w", snapshotID, err)
	}
	return false, nil
}

// copySnapshotOnVolume duplicates srcSnapshotID as dstSnapshotID by opening
// (or reusing) a VB on volumeID and calling viperblock.CopySnapshotMeta,
// which requires the VB to be opened over the snapshot's own source volume.
// Mirrors snapshotVolumeEngine: prefer the already-mounted VB over opening a
// second engine on the same volume, which would be a second writer.
func copySnapshotOnVolume(cfg *Config, volumeID, srcSnapshotID, dstSnapshotID string) (*ebsprovider.Snapshot, error) {
	if mv, ok := findMountedVolume(cfg, volumeID); ok && mv.VB != nil {
		return copySnapshotMetaWithVB(mv.VB, volumeID, srcSnapshotID, dstSnapshotID)
	}

	vb, err := openVolumeVB(cfg, volumeID)
	if err != nil {
		return nil, err
	}
	defer func() {
		vb.StopChunkUploader()
		vb.StopWALSyncer()
	}()
	return copySnapshotMetaWithVB(vb, volumeID, srcSnapshotID, dstSnapshotID)
}

// copySnapshotMetaWithVB runs the destination-exists probe and the actual
// copy against an already-resolved VB, shared by copySnapshotOnVolume's
// mounted and freshly-opened branches.
func copySnapshotMetaWithVB(vb *viperblock.VB, volumeID, srcSnapshotID, dstSnapshotID string) (*ebsprovider.Snapshot, error) {
	exists, err := snapshotConfigExists(vb, dstSnapshotID)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, errSnapshotDestinationExists
	}

	dst, err := vb.CopySnapshotMeta(srcSnapshotID, dstSnapshotID)
	if err != nil {
		return nil, fmt.Errorf("copy snapshot meta: %w", err)
	}
	return &ebsprovider.Snapshot{
		ID:             dstSnapshotID,
		SourceVolumeID: volumeID,
		SizeBytes:      utils.SafeUint64ToInt64(vb.GetVolumeSize()),
		CreatedAt:      dst.CreatedAt.UTC(),
		State:          ebsprovider.SnapshotStateCompleted,
		Handle:         snapshotHandle(dstSnapshotID),
	}, nil
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

	// handleCreateSnapshot serves both the wildcard queue subject (volume ID
	// is the whole suffix) and the owner subject (volume ID is the first
	// token); try the queue shape first since it is the common case.
	subjectVolumeID := strings.TrimPrefix(msg.Subject, ebsprovider.SnapshotCreateSubjectPrefix)
	if subjectVolumeID == msg.Subject {
		if ownerVolumeID, _, ok := ebsprovider.ParseOwnerSubject(msg.Subject); ok {
			subjectVolumeID = ownerVolumeID
		}
	}
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
		switch {
		case errors.Is(err, viperblock.ErrStateNotFound):
			code = ebsprovider.ErrorCodeNotFound
		case errors.Is(err, errSnapshotExistsElsewhere):
			code = ebsprovider.ErrorCodeAlreadyExists
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

// checkSnapshotIDFree refuses a snapshot ID that already exists unless this
// volume is the one it was taken from. An existing snapshot whose identity
// cannot be read is refused too: unprovable ownership must not overwrite it.
func checkSnapshotIDFree(vb *viperblock.VB, volumeID, snapshotID string) error {
	exists, err := snapshotConfigExists(vb, snapshotID)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if _, ident, err := vb.LoadSnapshotBlockMap(snapshotID); err == nil && ident.SourceVolumeName == volumeID {
		return nil
	}
	return fmt.Errorf("%w: %s", errSnapshotExistsElsewhere, snapshotID)
}

// snapshotVolumeEngine creates snapshotID off volumeID's live checkpoint,
// preferring an already-mounted VB over opening a second engine on the same
// volume. Draining the volume so the checkpoint is current is the caller's
// responsibility (see handleCreateSnapshot).
func snapshotVolumeEngine(cfg *Config, volumeID, snapshotID string) (*ebsprovider.Snapshot, error) {
	if mv, ok := findMountedVolume(cfg, volumeID); ok && mv.VB != nil {
		if err := checkSnapshotIDFree(mv.VB, volumeID, snapshotID); err != nil {
			return nil, err
		}
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
	if err := checkSnapshotIDFree(vb, volumeID, snapshotID); err != nil {
		return nil, err
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

// ownerVerbHandlers pairs each owner-routable verb's subject builder with
// the exact handler its queue subject already dispatches to, so mounted vs.
// unmounted routing differs only in which node answers, never in behavior.
func ownerVerbHandlers(cfg *Config, nc *nats.Conn) []struct {
	subjectFn func(string) (string, error)
	handler   nats.MsgHandler
} {
	return []struct {
		subjectFn func(string) (string, error)
		handler   nats.MsgHandler
	}{
		{ebsprovider.SnapshotCreateOwnerSubject, func(msg *nats.Msg) { handleCreateSnapshot(cfg, nc, msg) }},
		{ebsprovider.SnapshotCopyOwnerSubject, func(msg *nats.Msg) { handleCopySnapshot(cfg, msg) }},
		{ebsprovider.ExpandVolumeOwnerSubject, func(msg *nats.Msg) { handleExpandVolume(cfg, msg) }},
		{ebsprovider.GetVolumeOwnerSubject, func(msg *nats.Msg) { handleGetVolume(cfg, msg) }},
	}
}

// subscribeOwnerSubjects registers volumeName's owner subjects as plain,
// non-queue subscriptions, so a request for a mounted volume reaches this node
// rather than a random worker. A failure is logged and skipped, as ConfigSub is.
func subscribeOwnerSubjects(ctx context.Context, cfg *Config, nc *nats.Conn, volumeName string) []*nats.Subscription {
	var subs []*nats.Subscription
	for _, o := range ownerVerbHandlers(cfg, nc) {
		subject, err := o.subjectFn(volumeName)
		if err != nil {
			slog.ErrorContext(ctx, "Failed to build owner subject", "volume", volumeName, "err", err)
			continue
		}
		sub, err := nc.Subscribe(subject, o.handler)
		if err != nil {
			slog.ErrorContext(ctx, "Failed to subscribe to owner subject", "subject", subject, "volume", volumeName, "err", err)
			continue
		}
		subs = append(subs, sub)
	}
	return subs
}

// unsubscribeOwnerSubjects tears down subs, logging (not failing) any
// individual Unsubscribe error, matching ConfigSub's teardown handling.
func unsubscribeOwnerSubjects(volumeName string, subs []*nats.Subscription) {
	for _, sub := range subs {
		if sub == nil {
			continue
		}
		if err := sub.Unsubscribe(); err != nil {
			slog.Error("Failed to unsubscribe owner subject", "volume", volumeName, "err", err)
		}
	}
}

// constructMountedVB builds and opens the daemon-side viperblock engine: cache
// sizing (0 for -efi), StopChunkUploader (nbdkit owns the data path),
// Backend.Init, LoadState. Shared by mountVolume and startup recovery.
func constructMountedVB(ctx context.Context, cfg *Config, volumeName string) (*viperblock.VB, int, error) {
	s3cfg := vbs3.S3Config{
		VolumeName: volumeName,
		Bucket:     cfg.Bucket,
		Region:     cfg.Region,
		AccessKey:  cfg.AccessKey,
		SecretKey:  cfg.SecretKey,
		Host:       admin.DialTarget(cfg.S3Host),
	}

	// TODO: Improve based on system availability. Default 128MB cache
	defaultCache := (128 * 1024 * 1024) / int(viperblock.DefaultBlockSize)

	vbconfig := viperblock.VB{
		VolumeName: volumeName,
		VolumeSize: 1, // Workaround, calculated on LoadState()
		BaseDir:    cfg.BaseDir,
		Cache: viperblock.Cache{
			Config: viperblock.CacheConfig{
				// TODO: Improve, based on system memory
				Size: defaultCache,
			},
		},
		VolumeConfig:      viperblock.VolumeConfig{},
		MasterKey:         cfg.masterKey,
		EncryptionEnabled: cfg.masterKey != nil,
	}

	// Checked before the cache sizing below: viperblock.New returns a nil VB
	// alongside its error, so calling SetCacheSize first panics the caller.
	vb, err := viperblock.New(&vbconfig, "s3", s3cfg)
	if err != nil {
		return nil, 0, fmt.Errorf("connect to viperblock store: %w", err)
	}

	// New starts the chunk uploader and WAL syncer, so a failure below must
	// release them: the caller gets no handle to stop them with.
	opened := false
	defer func() {
		if !opened {
			vb.StopChunkUploader()
			vb.StopWALSyncer()
		}
	}()

	// Enable 128MB cache for main volumes, disable for efi (small, rarely read)
	// This cacheSize is passed to nbdkit plugin (separate viperblock instance)
	var nbdCacheSize int
	if strings.HasSuffix(volumeName, "-efi") {
		slog.InfoContext(ctx, "Disabling cache for auxiliary volume", "volume", volumeName)
		if err := vb.SetCacheSize(0, 0); err != nil {
			slog.ErrorContext(ctx, "Failed to set cache size", "err", err)
		}
		nbdCacheSize = 0
	} else {
		slog.InfoContext(ctx, "Enabling 128MB cache for main volume", "volume", volumeName, "blocks", defaultCache)
		if err := vb.SetCacheSize(defaultCache, 0); err != nil {
			slog.ErrorContext(ctx, "Failed to set cache size", "err", err)
		}
		nbdCacheSize = defaultCache
	}

	// This daemon-side VB tracks state only; the nbdkit plugin process owns
	// the data path and its own uploader. Stop this VB's background uploader
	// so it cannot overwrite the live checkpoint every 30s (AEAD corruption).
	vb.StopChunkUploader()

	if cfg.Debug {
		vb.SetDebug(true)
	}

	if err := vb.Backend.Init(); err != nil {
		return nil, 0, err
	}

	// Retry on transient backend errors so daemon recovery doesn't tip a healthy volume into cleanup.
	if err := loadStateWithRetry(vb, volumeName); err != nil {
		return nil, 0, err
	}

	opened = true
	return vb, nbdCacheSize, nil
}

// mountVolume is launchService's ebs.mount body, extracted so the legacy
// ebs.mount handler and the ebs.provider.v1.*.mount handler share one nbdkit
// mount path instead of risking two divergent implementations of it. It
// starts (or fails to start) nbdkit for volumeName and registers the result
// in cfg.MountedVolumes; it does not publish a response, that stays with the
// caller.
func mountVolume(ctx context.Context, cfg *Config, nc *nats.Conn, volumeName string, readOnly bool) (types.EBSMountResponse, error) {
	// Clear any receipt left by a previous mount before anything else can
	// return early, so a stale receipt can never survive into this mount.
	clearStaleSealReceipt(cfg.BaseDir, volumeName)

	_, mountSpan := otel.Tracer(viperblockdTracerName).Start(ctx, "ebs.mount",
		trace.WithAttributes(attribute.String("volume.id", volumeName)))

	var ebsResponse types.EBSMountResponse
	ebsResponse.Mounted = false
	defer func() { endSpanWithResponseError(mountSpan, ebsResponse.Error) }()

	vb, nbdCacheSize, err := cfg.buildVB(ctx, volumeName)
	if err != nil {
		ebsResponse.Error = err.Error()
		return ebsResponse, err
	}
	mountSpan.SetAttributes(attribute.Int64("volume.size_bytes", utils.SafeUint64ToInt64(vb.GetVolumeSize())))

	useTCP := cfg.NBDTransport == types.NBDTransportTCP

	var nbdURI string
	var nbdSocket string
	var nbdPort int

	if useTCP {
		// TCP transport - find a free port
		portStr, err := viperblock.FindFreePort()
		if err != nil {
			ebsResponse.Error = err.Error()
			return ebsResponse, err
		}

		// Parse the port from the address
		parts := strings.Split(portStr, ":")
		nbdPort, err = strconv.Atoi(parts[len(parts)-1])
		if err != nil {
			slog.ErrorContext(ctx, "Failed to convert port to int", "err", err)
			ebsResponse.Error = fmt.Sprintf("failed to parse port: %v", err)
			return ebsResponse, err
		}

		nbdURI = utils.FormatNBDTCPURI("127.0.0.1", nbdPort)
		slog.InfoContext(ctx, "Mounting volume (TCP)", "name", volumeName, "port", nbdPort, "uri", nbdURI)
	} else {
		// Unix socket transport (default) - generate unique socket path
		nbdSocket, err = utils.GenerateUniqueSocketFile(volumeName)
		if err != nil {
			ebsResponse.Error = err.Error()
			return ebsResponse, err
		}

		nbdURI = utils.FormatNBDSocketURI(nbdSocket)
		slog.InfoContext(ctx, "Mounting volume (socket)", "name", volumeName, "socket", nbdSocket, "uri", nbdURI)
	}

	// Generate PID file for nbdkit process
	nbdPidFile, err := utils.GeneratePidFile(fmt.Sprintf("nbdkit-vol-%s", volumeName))
	if err != nil {
		slog.ErrorContext(ctx, "Failed to generate nbdkit pid file", "err", err)
		ebsResponse.Error = fmt.Sprintf("failed to generate pid file: %v", err)
		return ebsResponse, err
	}

	nbdConfig := nbd.NBDKitConfig{
		Port:              nbdPort,
		Socket:            nbdSocket,
		UseTCP:            useTCP,
		PidFile:           nbdPidFile,
		PluginPath:        cfg.PluginPath,
		BaseDir:           cfg.BaseDir,
		Host:              admin.DialTarget(cfg.S3Host),
		Verbose:           false,
		Size:              utils.SafeUint64ToInt64(vb.GetVolumeSize()),
		Volume:            volumeName,
		Bucket:            cfg.Bucket,
		Region:            cfg.Region,
		AccessKey:         cfg.AccessKey,
		SecretKey:         cfg.SecretKey,
		CacheSize:         nbdCacheSize,
		ShardWAL:          cfg.ShardWAL,
		GCEnabled:         cfg.GCEnabled,
		EncryptionKeyFile: cfg.EncryptionKeyFile,
		ReadOnly:          readOnly,
	}

	// Create a unique error channel for this specific mount request
	processChan := make(chan int, 1)
	exitChan := make(chan int, 1)

	// TODO: Improve, use a process manager to track the (multiple) nbdkit process
	go func() {
		slog.Debug("Executing nbdkit")

		cmd, err := nbdConfig.Execute()
		if err != nil {
			slog.Error("Failed to execute nbdkit", "err", err)
			// Signal error (no PID) to parent goroutine
			processChan <- 0
			return
		}

		pid := cmd.Process.Pid
		// Signal successful startup w/ PID
		processChan <- pid

		err = cmd.Wait()

		if err != nil {
			slog.Error("Failed to wait for nbdkit", "err", err)
			exitChan <- 1
			return
		}

		exitCode := cmd.ProcessState.ExitCode()

		exitChan <- exitCode

		slog.Error("NBDKit exited", "code", exitCode)
	}()

	pid := <-processChan

	if pid == 0 {
		ebsResponse.Error = "Failed to start nbdkit"
		return ebsResponse, errors.New(ebsResponse.Error)
	}

	// Poll for up to 1 second to confirm nbdkit is running, checking every
	// 50ms so a fast failure is detected quickly instead of always
	// costing the full second. Any exit within that window means NBDKit
	// failed to stay up.
	const nbdkitConfirmPollInterval = 50 * time.Millisecond
	const nbdkitConfirmDeadline = 1 * time.Second
	confirmDeadline := time.Now().Add(nbdkitConfirmDeadline)
	for {
		select {
		case exitErr := <-exitChan:
			ebsResponse.Error = fmt.Sprintf("nbdkit exited unexpectedly (code=%d)", exitErr)
			return ebsResponse, errors.New(ebsResponse.Error)
		default:
		}

		if time.Now().After(confirmDeadline) {
			break
		}

		time.Sleep(nbdkitConfirmPollInterval)
	}

	// nbdkit is still running after the poll window, which means it started successfully
	slog.InfoContext(ctx, "NBDKit started successfully and is running")

	// NBDKit creates the socket with its own umask (typically 0755).
	// The daemon (different user, same group) needs write access to connect.
	if nbdSocket != "" {
		if err := os.Chmod(nbdSocket, 0770); err != nil { //nolint:gosec // socket needs group-write for cross-service access
			slog.WarnContext(ctx, "Failed to chmod NBD socket", "socket", nbdSocket, "err", err)
		}
	}

	ebsResponse.Mounted = true
	ebsResponse.URI = nbdURI

	// Subscribe to volume-specific config-update topic so encrypted-volume
	// metadata writes route to this node's live VB (the StateSeqNum owner).
	configSub, err := nc.Subscribe(fmt.Sprintf("ebs.config.%s", volumeName), makeConfigUpdateHandler(vb, volumeName))
	if err != nil {
		slog.ErrorContext(ctx, "Failed to subscribe to volume config topic", "volume", volumeName, "err", err)
	}

	// Owner-routed subscriptions let snapshot/expand/describe requests reach
	// this node directly while it holds the live engine, instead of the
	// spinifex-workers queue group delivering them to a random node.
	ownerSubs := subscribeOwnerSubjects(ctx, cfg, nc, volumeName)

	cfg.mu.Lock()
	cfg.MountedVolumes = append(cfg.MountedVolumes, MountedVolume{
		Name:      volumeName,
		Port:      nbdPort,
		Socket:    nbdSocket,
		NBDURI:    nbdURI,
		PID:       pid,
		VB:        vb,
		ConfigSub: configSub,
		OwnerSubs: ownerSubs,
		ReadOnly:  readOnly,
	})
	cfg.mu.Unlock()

	return ebsResponse, nil
}

// unmountVolume is launchService's ebs.unmount body, extracted for the same
// reason as mountVolume: the legacy ebs.unmount handler and the
// ebs.provider.v1.*.unmount handler must share the one seal-decision path
// rather than risk it diverging between two copies.
//
// A failed seal leaves volumeName in cfg.MountedVolumes (response.Error set,
// response.Mounted stays false but the entry is not removed) so a retry
// re-attempts the seal; a volume with no matching entry gets
// response.NotFound set instead of a bare error, so callers can tell "never
// mounted here" apart from "seal failed".
func unmountVolume(ctx context.Context, cfg *Config, volumeName string) (types.EBSUnMountResponse, error) {
	_, unmountSpan := otel.Tracer(viperblockdTracerName).Start(ctx, "ebs.unmount",
		trace.WithAttributes(attribute.String("volume.id", volumeName)))

	// Find the volume and extract references while holding the lock,
	// then release before calling VB.Close() (which does heavy S3 I/O).
	var ebsResponse types.EBSUnMountResponse
	defer func() { endSpanWithResponseError(unmountSpan, ebsResponse.Error) }()
	var matched MountedVolume
	var matchIdx = -1
	cfg.mu.Lock()
	for i, volume := range cfg.MountedVolumes {
		if volume.Name == volumeName {
			matched = volume
			matchIdx = i
			break
		}
	}
	cfg.mu.Unlock()

	if matchIdx >= 0 {
		ebsResponse = types.EBSUnMountResponse{
			Volume:  matched.Name,
			Mounted: false,
		}

		// Unsubscribe from volume-specific config-update topic
		if matched.ConfigSub != nil {
			if err := matched.ConfigSub.Unsubscribe(); err != nil {
				slog.ErrorContext(ctx, "Failed to unsubscribe config topic", "volume", volumeName, "err", err)
			}
		}
		unsubscribeOwnerSubjects(matched.Name, matched.OwnerSubs)

		// Stop background goroutines on the state-tracking VB.
		// Actual I/O is in the nbdkit plugin process; sealVolumeVB below
		// opens a fresh VB and calls Close() for the proper seal.
		if matched.VB != nil {
			matched.VB.StopChunkUploader()
			matched.VB.StopWALSyncer()
		}

		if err := utils.KillProcess(matched.PID); err != nil {
			slog.ErrorContext(ctx, "Failed to kill nbdkit process", "pid", matched.PID, "err", err)
		}

		// nbdkit is now dead, so no process writes the shared BaseDir: seal
		// the block map to predastore for volumes that hold local state to
		// flush (see volumeNeedsSeal).
		if isAuxVolume(matched.Name) {
			// Auxiliary volumes carry no durable guest data, so there is
			// nothing to seal even when local state is present.
		} else if volumeNeedsSeal(matched.Name, cfg.BaseDir) {
			// Local state survived: the plugin's seal either failed or was
			// cut short, so this fallback is the real seal.
			if err := cfg.seal(matched.Name); err != nil {
				slog.ErrorContext(ctx, "ebs.unmount: failed to seal volume to predastore", "volume", matched.Name, "err", err)
				ebsResponse.Error = fmt.Sprintf("seal volume: %v", err)
			} else {
				slog.InfoContext(ctx, "ebs.unmount: volume sealed to predastore", "volume", matched.Name)
			}
		} else if consumeSealReceipt(cfg.BaseDir, matched.Name) {
			// Healthy path: the plugin sealed to predastore and removed
			// its local state itself, leaving this receipt as proof.
			slog.InfoContext(ctx, "ebs.unmount: volume already sealed by nbdkit plugin", "volume", matched.Name)
		} else {
			// A durable volume reached unmount with no local WAL and no
			// seal receipt: this node never held its state, so there is
			// nothing to seal. WARN since this can mask a durability gap
			// the seal would otherwise close.
			slog.WarnContext(ctx, "ebs.unmount: no local viperblock state for volume, skipping seal", "volume", matched.Name, "baseDir", cfg.BaseDir)
		}

		// Remove the socket file if using socket transport
		if matched.Socket != "" {
			slog.InfoContext(ctx, "Removing socket file", "socket", matched.Socket)
			if err := os.Remove(matched.Socket); err != nil && !os.IsNotExist(err) {
				slog.ErrorContext(ctx, "Failed to delete nbd socket", "err", err, "socket", matched.Socket)
			}
		}

		// Only drop the volume from MountedVolumes once the seal actually
		// succeeded (or none was needed): a failed seal must leave it
		// mounted so a retry re-attempts sealVolumeVB, rather than a
		// caller seeing "not found" and mistaking a failed seal for a
		// completed one.
		if ebsResponse.Error == "" {
			cfg.mu.Lock()
			for i, volume := range cfg.MountedVolumes {
				if volume.Name == matched.Name {
					cfg.MountedVolumes = append(cfg.MountedVolumes[:i], cfg.MountedVolumes[i+1:]...)
					break
				}
			}
			cfg.mu.Unlock()
		}
	}

	if matchIdx < 0 {
		ebsResponse = types.EBSUnMountResponse{
			Volume:   volumeName,
			Error:    fmt.Sprintf("Volume %s not found", volumeName),
			NotFound: true,
		}
	}

	if ebsResponse.Error != "" {
		return ebsResponse, errors.New(ebsResponse.Error)
	}
	return ebsResponse, nil
}

// handlePublishVolume serves ebs.provider.v1.<node>.mount, the provider-neutral
// front for the same nbdkit mount path ebs.mount uses. It is node-addressed
// (registerProviderSubjects only subscribes it when cfg.NodeName is set), so
// unlike the legacy handler there is no queue-group fallback to reason about.
func handlePublishVolume(cfg *Config, nc *nats.Conn, msg *nats.Msg) {
	// Carry the caller's trace context into the mount span, as ebs.mount does.
	// Attach latency is the thing this boundary exists to measure, so an
	// orphaned span here would lose exactly the timing that matters.
	ctx, span := utils.StartConsumerSpan(msg)
	defer span.End()

	var req ebsprovider.PublishVolumeRequest
	if err := json.Unmarshal(msg.Data, &req); err != nil {
		slog.Error("ebs.provider.volume.publish: bad request", "err", err)
		respondJSON(msg, ebsprovider.PublishVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: badRequestError(err)})
		return
	}
	if req.SchemaVersion != ebsprovider.SchemaVersion {
		slog.Error("ebs.provider.volume.publish: unsupported schema version", "version", req.SchemaVersion)
		respondJSON(msg, ebsprovider.PublishVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: unsupportedVersionError(req.SchemaVersion)})
		return
	}
	if !validVolumeName(req.VolumeID) {
		respondJSON(msg, ebsprovider.PublishVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: invalidArgumentError("invalid volume id %q", req.VolumeID)})
		return
	}
	// Idempotent republish: a volume this node already has mounted must not
	// start a second nbdkit against it (the double-writer hazard this
	// provider boundary exists to prevent). Match memory.go's behaviour.
	if mv, ok := findMountedVolume(cfg, req.VolumeID); ok {
		// Access mode is fixed when nbdkit starts, so a republish asking for
		// the other mode cannot be answered with the running export.
		if mv.ReadOnly != req.ReadOnly {
			respondJSON(msg, ebsprovider.PublishVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: volumeInUseError("volume %s is already published read_only=%t on this node", req.VolumeID, mv.ReadOnly)})
			return
		}
		slog.Info("ebs.provider.volume.publish: already published, returning existing attachment", "volume", req.VolumeID)
		respondJSON(msg, ebsprovider.PublishVolumeResponse{
			Versioned: ebsprovider.NewVersioned(),
			Published: &ebsprovider.PublishedVolume{
				VolumeID: req.VolumeID,
				NodeID:   cfg.NodeName,
				NBDURI:   mv.NBDURI,
			},
		})
		return
	}

	mountResp, err := mountVolume(ctx, cfg, nc, req.VolumeID, req.ReadOnly)
	if err != nil {
		utils.MarkSpanError(span, err)
		if errors.Is(err, viperblock.ErrStateNotFound) {
			respondJSON(msg, ebsprovider.PublishVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: notFoundError("volume %s not found", req.VolumeID)})
			return
		}
		slog.Error("ebs.provider.volume.publish: mount failed", "volume", req.VolumeID, "err", err)
		respondJSON(msg, ebsprovider.PublishVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: internalError("mount volume: %v", err)})
		return
	}

	slog.Info("ebs.provider.volume.publish: published", "volume", req.VolumeID, "node", cfg.NodeName)
	respondJSON(msg, ebsprovider.PublishVolumeResponse{
		Versioned: ebsprovider.NewVersioned(),
		Published: &ebsprovider.PublishedVolume{
			VolumeID: req.VolumeID,
			NodeID:   cfg.NodeName,
			NBDURI:   mountResp.URI,
		},
	})
}

// handleUnpublishVolume serves ebs.provider.v1.<node>.unmount, fronting the
// same seal-decision path ebs.unmount uses. A failed seal is reported as an
// error while leaving the volume in cfg.MountedVolumes (see unmountVolume),
// never as success.
func handleUnpublishVolume(cfg *Config, msg *nats.Msg) {
	ctx, span := utils.StartConsumerSpan(msg)
	defer span.End()

	var req ebsprovider.UnpublishVolumeRequest
	if err := json.Unmarshal(msg.Data, &req); err != nil {
		slog.Error("ebs.provider.volume.unpublish: bad request", "err", err)
		respondJSON(msg, ebsprovider.UnpublishVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: badRequestError(err)})
		return
	}
	if req.SchemaVersion != ebsprovider.SchemaVersion {
		slog.Error("ebs.provider.volume.unpublish: unsupported schema version", "version", req.SchemaVersion)
		respondJSON(msg, ebsprovider.UnpublishVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: unsupportedVersionError(req.SchemaVersion)})
		return
	}
	if !validVolumeName(req.VolumeID) {
		respondJSON(msg, ebsprovider.UnpublishVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: invalidArgumentError("invalid volume id %q", req.VolumeID)})
		return
	}

	unmountResp, err := unmountVolume(ctx, cfg, req.VolumeID)
	if err != nil {
		// Nothing mounted here is already the state unpublish asks for, so a
		// retry after a request that completed server-side converges rather
		// than erroring, matching the legacy path's unmountResponseError.
		if unmountResp.NotFound {
			slog.Info("ebs.provider.volume.unpublish: already unpublished", "volume", req.VolumeID, "node", cfg.NodeName)
			respondJSON(msg, ebsprovider.UnpublishVolumeResponse{Versioned: ebsprovider.NewVersioned()})
			return
		}
		utils.MarkSpanError(span, err)
		slog.Error("ebs.provider.volume.unpublish: unmount failed", "volume", req.VolumeID, "err", err)
		respondJSON(msg, ebsprovider.UnpublishVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: internalError("unmount volume: %v", err)})
		return
	}

	slog.Info("ebs.provider.volume.unpublish: unpublished", "volume", req.VolumeID, "node", cfg.NodeName)
	respondJSON(msg, ebsprovider.UnpublishVolumeResponse{Versioned: ebsprovider.NewVersioned()})
}
