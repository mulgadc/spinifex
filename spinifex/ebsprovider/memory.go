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

	m.mu.Lock()
	defer m.mu.Unlock()
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
	}
	return cloneVolume(&volume), nil
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

func (m *MemoryProvider) PublishVolume(_ context.Context, req PublishVolumeRequest) (*PublishedVolume, error) {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	if req.VolumeID == "" || req.NodeID == "" {
		return nil, fmt.Errorf("%w: volume and node IDs are required", ErrInvalidArgument)
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
		NBDURI:   "nbd:unix:/memory/" + req.VolumeID + ".sock",
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
