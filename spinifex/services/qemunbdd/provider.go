package qemunbdd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"

	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// nbdSharedClients bounds how many simultaneous NBD connections qemu-nbd
// accepts against one export. A volume has exactly one attaching client, so
// this stays at qemu-nbd's own default rather than inviting concurrent writers through the same socket.
const nbdSharedClients = 1

// capabilities is fixed: this provider offers real CoW clones and crash
// consistent snapshots but, unlike viperblockd, cannot expand a published
// volume or report sparse extents.
var capabilities = ebsprovider.Capabilities{
	CopyOnWriteClone:        true,
	CrashConsistentSnapshot: true,
	OnlineExpansion:         false,
	SparseExtentReporting:   false,
	VolumeSeeding:           true,
	OwnerRouting:            false,
}

// volumeMeta carries the request attributes CreateVolume's idempotency check
// needs but a qcow2 file has no field for: a bare image has no notion of
// availability zone or opaque caller parameters. Kept in memory only, so it does not survive a provider restart.
type volumeMeta struct {
	availabilityZone string
	parameters       []byte
}

// snapshotMeta records a snapshot's source volume. CreateSnapshot copies the
// volume file outright rather than leaving a backing-file pointer (the
// snapshot must survive the source volume's deletion), so this is the only place that link is recorded; it is memory-only like volumeMeta.
type snapshotMeta struct {
	sourceVolumeID string
}

// publication is a running qemu-nbd export: its socket, its node, and the PID
// captured from --pid-file so UnpublishVolume can stop it. qemu-nbd forks
// into the background on success (--fork), so this PID is the only handle this process retains on it.
type publication struct {
	nodeID     string
	socketPath string
	pid        int
}

// Provider is an EBSProvider backed by qcow2 files and qemu-nbd. One volume
// is one qcow2 file; a snapshot is an independent copy, not a chunk
// reference, so DeleteVolume never has to consult a snapshot the way viperblock does.
type Provider struct {
	baseDir string
	run     runner

	mu           sync.Mutex
	published    map[string]*publication
	volumeMeta   map[string]volumeMeta
	snapshotMeta map[string]snapshotMeta
}

var _ ebsprovider.EBSProvider = (*Provider)(nil)

// NewProvider creates a Provider rooted at baseDir, creating its volumes,
// snapshots, sockets and tmp subdirectories if they do not already exist.
func NewProvider(baseDir string) (*Provider, error) {
	return newProvider(baseDir, execRunner{})
}

func newProvider(baseDir string, run runner) (*Provider, error) {
	if baseDir == "" {
		return nil, fmt.Errorf("qemunbdd: base directory is required")
	}
	absBaseDir, err := filepath.Abs(baseDir)
	if err != nil {
		return nil, fmt.Errorf("qemunbdd: resolve base directory: %w", err)
	}
	for _, sub := range []string{"volumes", "snapshots", "sockets", "tmp"} {
		if err := os.MkdirAll(filepath.Join(absBaseDir, sub), 0o750); err != nil {
			return nil, fmt.Errorf("qemunbdd: create %s directory: %w", sub, err)
		}
	}
	return &Provider{
		baseDir:      absBaseDir,
		run:          run,
		published:    make(map[string]*publication),
		volumeMeta:   make(map[string]volumeMeta),
		snapshotMeta: make(map[string]snapshotMeta),
	}, nil
}

func (p *Provider) volumePath(volumeID string) string {
	return filepath.Join(p.baseDir, "volumes", volumeID+".qcow2")
}

func (p *Provider) snapshotPath(snapshotID string) string {
	return filepath.Join(p.baseDir, "snapshots", snapshotID+".qcow2")
}

func (p *Provider) socketPath(volumeID string) string {
	return filepath.Join(p.baseDir, "sockets", volumeID+".sock")
}

func (p *Provider) pidFilePath(volumeID string) string {
	return filepath.Join(p.baseDir, "sockets", volumeID+".pid")
}

// checkVersion mirrors ebsprovider's own unexported check: the wire schema
// version must match exactly, since this provider does not translate between
// versions.
func checkVersion(version uint16) error {
	if version != ebsprovider.SchemaVersion {
		return fmt.Errorf("%w: got %d, want %d", ebsprovider.ErrUnsupportedVersion, version, ebsprovider.SchemaVersion)
	}
	return nil
}

func validCapacityRange(cr ebsprovider.CapacityRange) bool {
	return cr.RequiredBytes > 0 && (cr.LimitBytes == 0 || cr.RequiredBytes <= cr.LimitBytes)
}

func (p *Provider) GetCapabilities(_ context.Context, req ebsprovider.GetCapabilitiesRequest) (*ebsprovider.GetCapabilitiesResponse, error) {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	return &ebsprovider.GetCapabilitiesResponse{Versioned: ebsprovider.NewVersioned(), Capabilities: capabilities}, nil
}

// qemuImgInfo is the subset of `qemu-img info --output=json` this provider
// reads: the image's logical size, and the backing file a CoW clone points
// at, which stands in for a Go-side "created from snapshot X" record.
type qemuImgInfo struct {
	VirtualSize     int64  `json:"virtual-size"`
	BackingFilename string `json:"backing-filename"`
}

func (p *Provider) imgInfo(ctx context.Context, path string) (*qemuImgInfo, error) {
	out, err := p.run.Run(ctx, "qemu-img", "info", "--output=json", path)
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", path, err)
	}
	var info qemuImgInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return nil, fmt.Errorf("parse qemu-img info for %s: %w", path, err)
	}
	return &info, nil
}

func (p *Provider) CreateVolume(ctx context.Context, req ebsprovider.CreateVolumeRequest) (*ebsprovider.Volume, error) {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	if req.VolumeID == "" || !validCapacityRange(req.CapacityRange) {
		return nil, fmt.Errorf("%w: invalid volume ID or capacity range", ebsprovider.ErrInvalidArgument)
	}
	if err := ebsprovider.ValidateSeedData(req.SeedData); err != nil {
		return nil, err
	}
	if int64(len(req.SeedData)) > req.CapacityRange.RequiredBytes {
		return nil, fmt.Errorf("%w: seed data is larger than the requested capacity", ebsprovider.ErrInvalidArgument)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	volPath := p.volumePath(req.VolumeID)
	expectedBacking := ""
	if req.SourceSnapshotID != "" {
		expectedBacking = p.snapshotPath(req.SourceSnapshotID)
	}

	if _, err := os.Stat(volPath); err == nil {
		info, err := p.imgInfo(ctx, volPath)
		if err != nil {
			return nil, err
		}
		meta := p.volumeMeta[req.VolumeID]
		if info.VirtualSize != req.CapacityRange.RequiredBytes ||
			meta.availabilityZone != req.AvailabilityZone ||
			info.BackingFilename != expectedBacking ||
			!bytes.Equal(meta.parameters, req.Parameters) {
			return nil, fmt.Errorf("%w: volume %s", ebsprovider.ErrAlreadyExists, req.VolumeID)
		}
		return p.buildVolume(req.VolumeID, info.VirtualSize, meta.availabilityZone), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	sizeArg := strconv.FormatInt(req.CapacityRange.RequiredBytes, 10)
	switch {
	case req.SourceSnapshotID != "":
		snapPath := p.snapshotPath(req.SourceSnapshotID)
		snapInfo, err := p.statSnapshot(ctx, snapPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("%w: snapshot %s", ebsprovider.ErrNotFound, req.SourceSnapshotID)
			}
			return nil, err
		}
		if req.CapacityRange.RequiredBytes < snapInfo.VirtualSize {
			return nil, fmt.Errorf("%w: requested capacity is smaller than snapshot %s", ebsprovider.ErrInvalidArgument, req.SourceSnapshotID)
		}
		if _, err := p.run.Run(ctx, "qemu-img", "create", "-f", "qcow2", "-b", snapPath, "-F", "qcow2", volPath, sizeArg); err != nil {
			return nil, fmt.Errorf("create volume %s from snapshot %s: %w", req.VolumeID, req.SourceSnapshotID, err)
		}

	case req.SourceVolumeID != "":
		srcPath := p.volumePath(req.SourceVolumeID)
		if _, err := os.Stat(srcPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("%w: volume %s", ebsprovider.ErrNotFound, req.SourceVolumeID)
			}
			return nil, err
		}
		if _, err := p.run.Run(ctx, "qemu-img", "convert", "-O", "qcow2", srcPath, volPath); err != nil {
			return nil, fmt.Errorf("create volume %s from volume %s: %w", req.VolumeID, req.SourceVolumeID, err)
		}
		// qemu-img convert sizes the copy to its source, not to
		// CapacityRange.RequiredBytes, so a copy into a larger volume needs a
		// follow-on resize the contract does not separately account for.
		srcInfo, err := p.imgInfo(ctx, volPath)
		if err != nil {
			return nil, err
		}
		if req.CapacityRange.RequiredBytes > srcInfo.VirtualSize {
			if _, err := p.run.Run(ctx, "qemu-img", "resize", volPath, sizeArg); err != nil {
				return nil, fmt.Errorf("expand volume %s copy to requested capacity: %w", req.VolumeID, err)
			}
		}

	default:
		if _, err := p.run.Run(ctx, "qemu-img", "create", "-f", "qcow2", volPath, sizeArg); err != nil {
			return nil, fmt.Errorf("create volume %s: %w", req.VolumeID, err)
		}
	}

	if err := p.writeSeed(ctx, volPath, req.SeedData); err != nil {
		_ = os.Remove(volPath)
		return nil, err
	}

	p.volumeMeta[req.VolumeID] = volumeMeta{availabilityZone: req.AvailabilityZone, parameters: bytes.Clone(req.Parameters)}
	return p.buildVolume(req.VolumeID, req.CapacityRange.RequiredBytes, req.AvailabilityZone), nil
}

// statSnapshot resolves os.ErrNotExist through os.Stat before shelling out,
// so a missing source snapshot is reported without invoking qemu-img on a
// path that cannot exist.
func (p *Provider) statSnapshot(ctx context.Context, snapPath string) (*qemuImgInfo, error) {
	if _, err := os.Stat(snapPath); err != nil {
		return nil, err
	}
	return p.imgInfo(ctx, snapPath)
}

// writeSeed stages SeedData to a temporary file and writes it at offset 0
// with qemu-io. qemu-img has no operation for arbitrary byte writes, and
// qemu-io's write command reads its buffer from a file (-s), not an inline argument, so the bytes must round-trip through disk first.
func (p *Provider) writeSeed(ctx context.Context, volPath string, seed []byte) error {
	if len(seed) == 0 {
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Join(p.baseDir, "tmp"), "seed-*.bin")
	if err != nil {
		return fmt.Errorf("stage seed data: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(seed); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("stage seed data: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("stage seed data: %w", err)
	}
	writeCmd := fmt.Sprintf("write -s %s 0 %d", tmpPath, len(seed))
	if _, err := p.run.Run(ctx, "qemu-io", "-f", "qcow2", "-c", writeCmd, volPath); err != nil {
		return fmt.Errorf("write seed data: %w", err)
	}
	return nil
}

// buildVolume must be called with p.mu held: it consults the published map.
func (p *Provider) buildVolume(volumeID string, size int64, az string) *ebsprovider.Volume {
	state := ebsprovider.VolumeStateAvailable
	if _, ok := p.published[volumeID]; ok {
		state = ebsprovider.VolumeStateInUse
	}
	return &ebsprovider.Volume{
		ID:               volumeID,
		CapacityBytes:    size,
		State:            state,
		Handle:           p.volumePath(volumeID),
		AvailabilityZone: az,
	}
}

func (p *Provider) GetVolume(ctx context.Context, req ebsprovider.GetVolumeRequest) (*ebsprovider.Volume, error) {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	if req.VolumeID == "" {
		return nil, fmt.Errorf("%w: volume ID is required", ebsprovider.ErrInvalidArgument)
	}
	volPath := p.volumePath(req.VolumeID)
	if _, err := os.Stat(volPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: volume %s", ebsprovider.ErrNotFound, req.VolumeID)
		}
		return nil, err
	}
	if req.Handle != "" && req.Handle != volPath {
		return nil, fmt.Errorf("%w: volume %s", ebsprovider.ErrNotFound, req.VolumeID)
	}
	info, err := p.imgInfo(ctx, volPath)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	meta := p.volumeMeta[req.VolumeID]
	return p.buildVolume(req.VolumeID, info.VirtualSize, meta.availabilityZone), nil
}

func (p *Provider) ExpandVolume(ctx context.Context, req ebsprovider.ExpandVolumeRequest) (*ebsprovider.Volume, error) {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	if req.VolumeID == "" || !validCapacityRange(req.CapacityRange) {
		return nil, fmt.Errorf("%w: invalid volume ID or capacity range", ebsprovider.ErrInvalidArgument)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	volPath := p.volumePath(req.VolumeID)
	if _, err := os.Stat(volPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: volume %s", ebsprovider.ErrNotFound, req.VolumeID)
		}
		return nil, err
	}
	if req.Handle != "" && req.Handle != volPath {
		return nil, fmt.Errorf("%w: volume %s", ebsprovider.ErrNotFound, req.VolumeID)
	}
	info, err := p.imgInfo(ctx, volPath)
	if err != nil {
		return nil, err
	}
	if req.CapacityRange.RequiredBytes < info.VirtualSize {
		return nil, fmt.Errorf("%w: volume expansion is grow-only", ebsprovider.ErrInvalidArgument)
	}
	// ebsprovider/errors.go has no failed-precondition sentinel, so a
	// published volume this provider cannot expand online is reported as
	// ErrVolumeInUse, matching memory.go's identical refusal.
	if _, published := p.published[req.VolumeID]; published && req.CapacityRange.RequiredBytes > info.VirtualSize {
		return nil, fmt.Errorf("%w: provider does not support online expansion", ebsprovider.ErrVolumeInUse)
	}
	if req.CapacityRange.RequiredBytes != info.VirtualSize {
		if _, err := p.run.Run(ctx, "qemu-img", "resize", volPath, strconv.FormatInt(req.CapacityRange.RequiredBytes, 10)); err != nil {
			return nil, fmt.Errorf("expand volume %s: %w", req.VolumeID, err)
		}
	}
	meta := p.volumeMeta[req.VolumeID]
	return p.buildVolume(req.VolumeID, req.CapacityRange.RequiredBytes, meta.availabilityZone), nil
}

func (p *Provider) DeleteVolume(_ context.Context, req ebsprovider.DeleteVolumeRequest) error {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return err
	}
	if req.VolumeID == "" {
		return fmt.Errorf("%w: volume ID is required", ebsprovider.ErrInvalidArgument)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	volPath := p.volumePath(req.VolumeID)
	if _, err := os.Stat(volPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if req.Handle != "" && req.Handle != volPath {
		return nil
	}
	if _, published := p.published[req.VolumeID]; published {
		return fmt.Errorf("%w: volume %s", ebsprovider.ErrVolumeInUse, req.VolumeID)
	}
	if err := os.Remove(volPath); err != nil {
		return fmt.Errorf("delete volume %s: %w", req.VolumeID, err)
	}
	delete(p.volumeMeta, req.VolumeID)
	return nil
}

func (p *Provider) buildSnapshot(ctx context.Context, snapshotID, sourceVolumeID, path string) (*ebsprovider.Snapshot, error) {
	info, err := p.imgInfo(ctx, path)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat snapshot %s: %w", snapshotID, err)
	}
	return &ebsprovider.Snapshot{
		ID:             snapshotID,
		SourceVolumeID: sourceVolumeID,
		SizeBytes:      info.VirtualSize,
		CreatedAt:      fi.ModTime().UTC(),
		State:          ebsprovider.SnapshotStateCompleted,
		Handle:         path,
	}, nil
}

func (p *Provider) CreateSnapshot(ctx context.Context, req ebsprovider.CreateSnapshotRequest) (*ebsprovider.Snapshot, error) {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	if req.SnapshotID == "" || req.VolumeID == "" {
		return nil, fmt.Errorf("%w: snapshot and volume IDs are required", ebsprovider.ErrInvalidArgument)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	snapPath := p.snapshotPath(req.SnapshotID)
	if meta, ok := p.snapshotMeta[req.SnapshotID]; ok {
		if meta.sourceVolumeID != req.VolumeID {
			return nil, fmt.Errorf("%w: snapshot %s", ebsprovider.ErrAlreadyExists, req.SnapshotID)
		}
		return p.buildSnapshot(ctx, req.SnapshotID, meta.sourceVolumeID, snapPath)
	}
	// A file on disk with no matching snapshotMeta entry means the provider
	// restarted since it was created: the source-volume link lived only in
	// memory and cannot be recovered, so the ID is refused rather than silently overwritten or accepted without provenance.
	if _, err := os.Stat(snapPath); err == nil {
		return nil, fmt.Errorf("%w: snapshot %s", ebsprovider.ErrAlreadyExists, req.SnapshotID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	volPath := p.volumePath(req.VolumeID)
	if _, err := os.Stat(volPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: volume %s", ebsprovider.ErrNotFound, req.VolumeID)
		}
		return nil, err
	}
	if req.VolumeHandle != "" && req.VolumeHandle != volPath {
		return nil, fmt.Errorf("%w: volume %s", ebsprovider.ErrNotFound, req.VolumeID)
	}

	if _, err := p.run.Run(ctx, "qemu-img", "convert", "-O", "qcow2", volPath, snapPath); err != nil {
		return nil, fmt.Errorf("create snapshot %s: %w", req.SnapshotID, err)
	}
	p.snapshotMeta[req.SnapshotID] = snapshotMeta{sourceVolumeID: req.VolumeID}
	return p.buildSnapshot(ctx, req.SnapshotID, req.VolumeID, snapPath)
}

// snapshotInUse reports whether any volume file still points at snapPath as
// its qcow2 backing file, the same relationship CreateVolume establishes for
// a SourceSnapshotID clone.
func (p *Provider) snapshotInUse(ctx context.Context, snapPath string) (bool, error) {
	volumesDir := filepath.Join(p.baseDir, "volumes")
	entries, err := os.ReadDir(volumesDir)
	if err != nil {
		return false, fmt.Errorf("list volumes: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := p.imgInfo(ctx, filepath.Join(volumesDir, entry.Name()))
		if err != nil {
			return false, err
		}
		if info.BackingFilename == snapPath {
			return true, nil
		}
	}
	return false, nil
}

func (p *Provider) DeleteSnapshot(ctx context.Context, req ebsprovider.DeleteSnapshotRequest) error {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return err
	}
	if req.SnapshotID == "" {
		return fmt.Errorf("%w: snapshot ID is required", ebsprovider.ErrInvalidArgument)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	snapPath := p.snapshotPath(req.SnapshotID)
	if _, err := os.Stat(snapPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if req.Handle != "" && req.Handle != snapPath {
		return nil
	}
	inUse, err := p.snapshotInUse(ctx, snapPath)
	if err != nil {
		return err
	}
	if inUse {
		// Same sentinel-substitution as ExpandVolume/DeleteVolume: no
		// failed-precondition error exists, so a snapshot a volume still
		// backs onto is reported as ErrVolumeInUse.
		return fmt.Errorf("%w: snapshot %s is a backing file for another volume", ebsprovider.ErrVolumeInUse, req.SnapshotID)
	}
	if err := os.Remove(snapPath); err != nil {
		return fmt.Errorf("delete snapshot %s: %w", req.SnapshotID, err)
	}
	delete(p.snapshotMeta, req.SnapshotID)
	return nil
}

func (p *Provider) CopySnapshot(ctx context.Context, req ebsprovider.CopySnapshotRequest) (*ebsprovider.Snapshot, error) {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	if req.SourceSnapshotID == "" || req.DestinationSnapshotID == "" || req.VolumeID == "" {
		return nil, fmt.Errorf("%w: source snapshot, destination snapshot, and volume IDs are required", ebsprovider.ErrInvalidArgument)
	}
	if req.SourceSnapshotID == req.DestinationSnapshotID {
		return nil, fmt.Errorf("%w: destination snapshot must differ from the source", ebsprovider.ErrInvalidArgument)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	srcMeta, ok := p.snapshotMeta[req.SourceSnapshotID]
	if !ok {
		return nil, fmt.Errorf("%w: snapshot %s", ebsprovider.ErrNotFound, req.SourceSnapshotID)
	}
	if srcMeta.sourceVolumeID != req.VolumeID {
		return nil, fmt.Errorf("%w: snapshot %s belongs to volume %s, not %s", ebsprovider.ErrInvalidArgument, req.SourceSnapshotID, srcMeta.sourceVolumeID, req.VolumeID)
	}
	dstPath := p.snapshotPath(req.DestinationSnapshotID)
	if _, ok := p.snapshotMeta[req.DestinationSnapshotID]; ok {
		return nil, fmt.Errorf("%w: snapshot %s", ebsprovider.ErrAlreadyExists, req.DestinationSnapshotID)
	}
	// A stray file with no metadata entry is the same restart-orphan case
	// CreateSnapshot refuses: without provenance it cannot be told apart
	// from a legitimate destination, so it is treated as already existing.
	if _, err := os.Stat(dstPath); err == nil {
		return nil, fmt.Errorf("%w: snapshot %s", ebsprovider.ErrAlreadyExists, req.DestinationSnapshotID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	srcPath := p.snapshotPath(req.SourceSnapshotID)
	if err := copyFile(srcPath, dstPath); err != nil {
		return nil, fmt.Errorf("copy snapshot %s to %s: %w", req.SourceSnapshotID, req.DestinationSnapshotID, err)
	}
	p.snapshotMeta[req.DestinationSnapshotID] = snapshotMeta{sourceVolumeID: req.VolumeID}
	return p.buildSnapshot(ctx, req.DestinationSnapshotID, req.VolumeID, dstPath)
}

// copyFile duplicates a snapshot file directly: CopySnapshot's job is to
// produce a second, independent file, not a CoW clone, so this is plain
// filesystem I/O and never touches the runner.
func copyFile(srcPath, dstPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	return dst.Close()
}

func (p *Provider) PublishVolume(ctx context.Context, req ebsprovider.PublishVolumeRequest) (*ebsprovider.PublishedVolume, error) {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	if req.VolumeID == "" || req.NodeID == "" {
		return nil, fmt.Errorf("%w: volume and node IDs are required", ebsprovider.ErrInvalidArgument)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	volPath := p.volumePath(req.VolumeID)
	if _, err := os.Stat(volPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: volume %s", ebsprovider.ErrNotFound, req.VolumeID)
		}
		return nil, err
	}
	if req.Handle != "" && req.Handle != volPath {
		return nil, fmt.Errorf("%w: volume %s", ebsprovider.ErrNotFound, req.VolumeID)
	}
	if pub, ok := p.published[req.VolumeID]; ok {
		if pub.nodeID != req.NodeID {
			return nil, fmt.Errorf("%w: volume %s is published to %s", ebsprovider.ErrVolumeInUse, req.VolumeID, pub.nodeID)
		}
		return &ebsprovider.PublishedVolume{VolumeID: req.VolumeID, NodeID: pub.nodeID, NBDURI: utils.FormatNBDSocketURI(pub.socketPath)}, nil
	}

	sockPath := p.socketPath(req.VolumeID)
	pidPath := p.pidFilePath(req.VolumeID)
	_ = os.Remove(sockPath)
	_ = os.Remove(pidPath)

	args := []string{
		"--socket", sockPath,
		"--format", "qcow2",
		"--persistent",
		"--shared=" + strconv.Itoa(nbdSharedClients),
		"--fork",
		"--pid-file", pidPath,
	}
	if req.ReadOnly {
		args = append(args, "-r")
	}
	args = append(args, volPath)
	if _, err := p.run.Run(ctx, "qemu-nbd", args...); err != nil {
		return nil, fmt.Errorf("publish volume %s: %w", req.VolumeID, err)
	}

	pid := readPID(pidPath)
	p.published[req.VolumeID] = &publication{nodeID: req.NodeID, socketPath: sockPath, pid: pid}
	return &ebsprovider.PublishedVolume{VolumeID: req.VolumeID, NodeID: req.NodeID, NBDURI: utils.FormatNBDSocketURI(sockPath)}, nil
}

// readPID best-effort reads the PID qemu-nbd's --fork wrote to pidPath. A
// missing or unparsable file (as in tests, where no real qemu-nbd runs)
// yields 0, meaning UnpublishVolume will skip signaling a process.
func readPID(pidPath string) int {
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(string(bytes.TrimSpace(data)))
	if err != nil {
		return 0
	}
	return pid
}

func (p *Provider) UnpublishVolume(_ context.Context, req ebsprovider.UnpublishVolumeRequest) error {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return err
	}
	if req.VolumeID == "" || req.NodeID == "" {
		return fmt.Errorf("%w: volume and node IDs are required", ebsprovider.ErrInvalidArgument)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	pub, ok := p.published[req.VolumeID]
	if !ok || pub.nodeID != req.NodeID {
		return nil
	}
	if pub.pid > 0 {
		if proc, err := os.FindProcess(pub.pid); err == nil {
			_ = proc.Signal(syscall.SIGTERM)
		}
	}
	_ = os.Remove(pub.socketPath)
	_ = os.Remove(p.pidFilePath(req.VolumeID))
	delete(p.published, req.VolumeID)
	return nil
}
