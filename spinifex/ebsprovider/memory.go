package ebsprovider

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"
)

// MemoryProvider is a deterministic, concurrency-safe Provider used by
// control-plane tests. It implements the same idempotency rules expected of a
// real provider rather than acting as a programmable mock.
type MemoryProvider struct {
	mu           sync.RWMutex
	capabilities Capabilities
	volumes      map[string]*memoryVolume
	snapshots    map[string]*Snapshot
	now          func() time.Time
}

type memoryVolume struct {
	volume           Volume
	sourceSnapshotID string
	parameters       []byte
	seed             []byte
	published        *PublishedVolume
}

var _ EBSProvider = (*MemoryProvider)(nil)

func NewMemoryProvider(capabilities Capabilities) *MemoryProvider {
	return &MemoryProvider{
		capabilities: capabilities,
		volumes:      make(map[string]*memoryVolume),
		snapshots:    make(map[string]*Snapshot),
		now:          time.Now,
	}
}

func (m *MemoryProvider) GetCapabilities(_ context.Context, req GetCapabilitiesRequest) (*GetCapabilitiesResponse, error) {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	return &GetCapabilitiesResponse{Versioned: NewVersioned(), Capabilities: m.capabilities}, nil
}

func (m *MemoryProvider) CreateVolume(_ context.Context, req CreateVolumeRequest) (*Volume, error) {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	if req.VolumeID == "" || req.CapacityRange.RequiredBytes <= 0 ||
		(req.CapacityRange.LimitBytes > 0 && req.CapacityRange.RequiredBytes > req.CapacityRange.LimitBytes) {
		return nil, fmt.Errorf("%w: invalid volume ID or capacity range", ErrInvalidArgument)
	}
	if req.SourceSnapshotID != "" && req.SourceSnapshotVolumeID == "" {
		return nil, fmt.Errorf("%w: source snapshot volume ID is required with a source snapshot", ErrInvalidArgument)
	}
	if err := ValidateSeedData(req.SeedData); err != nil {
		return nil, err
	}
	if len(req.SeedData) > 0 && !m.capabilities.VolumeSeeding {
		return nil, fmt.Errorf("%w: volume seeding", ErrUnsupportedCapability)
	}
	if int64(len(req.SeedData)) > req.CapacityRange.RequiredBytes {
		return nil, fmt.Errorf("%w: seed data is larger than the requested capacity", ErrInvalidArgument)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	// SeedData is deliberately absent from the mismatch check below: a seed
	// applies only to a volume this call creates, so a repeated create returns
	// the existing volume rather than overwriting bytes the guest has since set.
	if existing := m.volumes[req.VolumeID]; existing != nil {
		if existing.volume.CapacityBytes != req.CapacityRange.RequiredBytes ||
			existing.volume.AvailabilityZone != req.AvailabilityZone ||
			existing.sourceSnapshotID != req.SourceSnapshotID ||
			!bytes.Equal(existing.parameters, req.Parameters) {
			return nil, fmt.Errorf("%w: volume %s", ErrAlreadyExists, req.VolumeID)
		}
		return cloneVolume(&existing.volume), nil
	}
	if req.SourceSnapshotID != "" {
		snapshot := m.snapshots[req.SourceSnapshotID]
		if snapshot == nil {
			return nil, fmt.Errorf("%w: snapshot %s", ErrNotFound, req.SourceSnapshotID)
		}
		if req.CapacityRange.RequiredBytes < snapshot.SizeBytes {
			return nil, fmt.Errorf("%w: requested capacity is smaller than snapshot %s", ErrInvalidArgument, req.SourceSnapshotID)
		}
		// The caller names the snapshot's origin volume, so a wrong name is a
		// caller bug the reference provider refuses rather than resolving
		// blocks against a volume the snapshot never came from.
		if req.SourceSnapshotVolumeID != snapshot.SourceVolumeID {
			return nil, fmt.Errorf("%w: snapshot %s was taken from volume %s, not %s",
				ErrInvalidArgument, req.SourceSnapshotID, snapshot.SourceVolumeID, req.SourceSnapshotVolumeID)
		}
	}

	volume := Volume{
		ID:               req.VolumeID,
		CapacityBytes:    req.CapacityRange.RequiredBytes,
		State:            VolumeStateAvailable,
		Handle:           "memory://volume/" + req.VolumeID,
		AvailabilityZone: req.AvailabilityZone,
	}
	m.volumes[req.VolumeID] = &memoryVolume{
		volume:           volume,
		sourceSnapshotID: req.SourceSnapshotID,
		parameters:       bytes.Clone(req.Parameters),
		seed:             bytes.Clone(req.SeedData),
	}
	return cloneVolume(&volume), nil
}

// SeedData returns the bytes a CreateVolume call seeded the volume with, so a
// control-plane test can assert what it asked the provider to write.
func (m *MemoryProvider) SeedData(volumeID string) ([]byte, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	volume := m.volumes[volumeID]
	if volume == nil {
		return nil, false
	}
	return bytes.Clone(volume.seed), true
}

func (m *MemoryProvider) GetVolume(_ context.Context, req GetVolumeRequest) (*Volume, error) {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	if req.VolumeID == "" {
		return nil, fmt.Errorf("%w: volume ID is required", ErrInvalidArgument)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	volume := m.volumes[req.VolumeID]
	if volume == nil || (req.Handle != "" && req.Handle != volume.volume.Handle) {
		return nil, fmt.Errorf("%w: volume %s", ErrNotFound, req.VolumeID)
	}
	return cloneVolume(&volume.volume), nil
}

func (m *MemoryProvider) ExpandVolume(_ context.Context, req ExpandVolumeRequest) (*Volume, error) {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	if req.VolumeID == "" || req.CapacityRange.RequiredBytes <= 0 ||
		(req.CapacityRange.LimitBytes > 0 && req.CapacityRange.RequiredBytes > req.CapacityRange.LimitBytes) {
		return nil, fmt.Errorf("%w: invalid volume ID or capacity range", ErrInvalidArgument)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	volume := m.volumes[req.VolumeID]
	if volume == nil || (req.Handle != "" && req.Handle != volume.volume.Handle) {
		return nil, fmt.Errorf("%w: volume %s", ErrNotFound, req.VolumeID)
	}
	if req.CapacityRange.RequiredBytes < volume.volume.CapacityBytes {
		return nil, fmt.Errorf("%w: volume expansion is grow-only", ErrInvalidArgument)
	}
	if volume.published != nil && !m.capabilities.OnlineExpansion && req.CapacityRange.RequiredBytes > volume.volume.CapacityBytes {
		return nil, fmt.Errorf("%w: provider does not support online expansion", ErrVolumeInUse)
	}
	volume.volume.CapacityBytes = req.CapacityRange.RequiredBytes
	return cloneVolume(&volume.volume), nil
}

func (m *MemoryProvider) DeleteVolume(_ context.Context, req DeleteVolumeRequest) error {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return err
	}
	if req.VolumeID == "" {
		return fmt.Errorf("%w: volume ID is required", ErrInvalidArgument)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	volume := m.volumes[req.VolumeID]
	if volume == nil {
		return nil
	}
	if req.Handle != "" && req.Handle != volume.volume.Handle {
		return nil
	}
	if volume.published != nil {
		return fmt.Errorf("%w: volume %s", ErrVolumeInUse, req.VolumeID)
	}
	delete(m.volumes, req.VolumeID)
	return nil
}

func (m *MemoryProvider) CreateSnapshot(_ context.Context, req CreateSnapshotRequest) (*Snapshot, error) {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	if req.SnapshotID == "" || req.VolumeID == "" {
		return nil, fmt.Errorf("%w: snapshot and volume IDs are required", ErrInvalidArgument)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing := m.snapshots[req.SnapshotID]; existing != nil {
		if existing.SourceVolumeID != req.VolumeID {
			return nil, fmt.Errorf("%w: snapshot %s", ErrAlreadyExists, req.SnapshotID)
		}
		return cloneSnapshot(existing), nil
	}
	volume := m.volumes[req.VolumeID]
	if volume == nil || (req.VolumeHandle != "" && req.VolumeHandle != volume.volume.Handle) {
		return nil, fmt.Errorf("%w: volume %s", ErrNotFound, req.VolumeID)
	}
	snapshot := &Snapshot{
		ID:             req.SnapshotID,
		SourceVolumeID: req.VolumeID,
		SizeBytes:      volume.volume.CapacityBytes,
		CreatedAt:      m.now().UTC(),
		State:          SnapshotStateCompleted,
		Handle:         "memory://snapshot/" + req.SnapshotID,
	}
	m.snapshots[req.SnapshotID] = snapshot
	return cloneSnapshot(snapshot), nil
}

// Snapshot reports what the provider holds under snapshotID, so a
// control-plane test can assert a snapshot really reached the provider rather
// than existing only as a control-plane document.
func (m *MemoryProvider) Snapshot(snapshotID string) (*Snapshot, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	snapshot := m.snapshots[snapshotID]
	if snapshot == nil {
		return nil, false
	}
	return cloneSnapshot(snapshot), true
}

func (m *MemoryProvider) DeleteSnapshot(_ context.Context, req DeleteSnapshotRequest) error {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return err
	}
	if req.SnapshotID == "" {
		return fmt.Errorf("%w: snapshot ID is required", ErrInvalidArgument)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	snapshot := m.snapshots[req.SnapshotID]
	if snapshot == nil || (req.Handle != "" && req.Handle != snapshot.Handle) {
		return nil
	}
	delete(m.snapshots, req.SnapshotID)
	return nil
}

// CopySnapshot duplicates SourceSnapshotID under DestinationSnapshotID.
// Mirrors viperblock.CopySnapshotMeta's contract: it refuses an existing
// destination, an empty or equal ID pair, and a snapshot that does not
// belong to VolumeID (the caller's claimed source volume).
func (m *MemoryProvider) CopySnapshot(_ context.Context, req CopySnapshotRequest) (*Snapshot, error) {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	if req.SourceSnapshotID == "" || req.DestinationSnapshotID == "" || req.VolumeID == "" {
		return nil, fmt.Errorf("%w: source snapshot, destination snapshot, and volume IDs are required", ErrInvalidArgument)
	}
	if req.SourceSnapshotID == req.DestinationSnapshotID {
		return nil, fmt.Errorf("%w: destination snapshot must differ from the source", ErrInvalidArgument)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	src := m.snapshots[req.SourceSnapshotID]
	if src == nil {
		return nil, fmt.Errorf("%w: snapshot %s", ErrNotFound, req.SourceSnapshotID)
	}
	if src.SourceVolumeID != req.VolumeID {
		return nil, fmt.Errorf("%w: snapshot %s belongs to volume %s, not %s", ErrInvalidArgument, req.SourceSnapshotID, src.SourceVolumeID, req.VolumeID)
	}
	if existing := m.snapshots[req.DestinationSnapshotID]; existing != nil {
		return nil, fmt.Errorf("%w: snapshot %s", ErrAlreadyExists, req.DestinationSnapshotID)
	}

	dst := &Snapshot{
		ID:             req.DestinationSnapshotID,
		SourceVolumeID: src.SourceVolumeID,
		SizeBytes:      src.SizeBytes,
		CreatedAt:      m.now().UTC(),
		State:          SnapshotStateCompleted,
		Handle:         "memory://snapshot/" + req.DestinationSnapshotID,
	}
	m.snapshots[req.DestinationSnapshotID] = dst
	return cloneSnapshot(dst), nil
}

func (m *MemoryProvider) PublishVolume(_ context.Context, req PublishVolumeRequest) (*PublishedVolume, error) {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	if req.VolumeID == "" || req.NodeID == "" {
		return nil, fmt.Errorf("%w: volume and node IDs are required", ErrInvalidArgument)
	}
	if req.ReadOnly && !m.capabilities.ReadOnlyPublish {
		return nil, fmt.Errorf("%w: read-only publication", ErrUnsupportedCapability)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	volume := m.volumes[req.VolumeID]
	if volume == nil || (req.Handle != "" && req.Handle != volume.volume.Handle) {
		return nil, fmt.Errorf("%w: volume %s", ErrNotFound, req.VolumeID)
	}
	if volume.published != nil {
		if volume.published.NodeID != req.NodeID {
			return nil, fmt.Errorf("%w: volume %s is published to %s", ErrVolumeInUse, req.VolumeID, volume.published.NodeID)
		}
		return clonePublished(volume.published), nil
	}
	volume.published = &PublishedVolume{
		VolumeID: req.VolumeID,
		NodeID:   req.NodeID,
		NBDURI:   "nbd+unix:///?socket=/memory/" + req.VolumeID + ".sock",
	}
	volume.volume.State = VolumeStateInUse
	return clonePublished(volume.published), nil
}

func (m *MemoryProvider) UnpublishVolume(_ context.Context, req UnpublishVolumeRequest) error {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return err
	}
	if req.VolumeID == "" || req.NodeID == "" {
		return fmt.Errorf("%w: volume and node IDs are required", ErrInvalidArgument)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	volume := m.volumes[req.VolumeID]
	if volume == nil || volume.published == nil || volume.published.NodeID != req.NodeID {
		return nil
	}
	volume.published = nil
	volume.volume.State = VolumeStateAvailable
	return nil
}

func cloneVolume(volume *Volume) *Volume {
	if volume == nil {
		return nil
	}
	clone := *volume
	clone.ProviderData = bytes.Clone(volume.ProviderData)
	return &clone
}

func cloneSnapshot(snapshot *Snapshot) *Snapshot {
	if snapshot == nil {
		return nil
	}
	clone := *snapshot
	return &clone
}

func clonePublished(published *PublishedVolume) *PublishedVolume {
	if published == nil {
		return nil
	}
	clone := *published
	return &clone
}
