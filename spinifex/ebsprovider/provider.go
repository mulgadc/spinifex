// Package ebsprovider defines Spinifex's provider-neutral block-storage
// contract. Provider implementations must not expose their engine's Go types
// through this package; provider-specific settings and handles stay opaque.
package ebsprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// SchemaVersion is the only wire-contract version understood by this build.
// Every request and response carries it so version skew fails at the boundary
// instead of silently decoding a changed payload.
const SchemaVersion uint16 = 1

// Versioned is embedded in every request and response sent over NATS.
type Versioned struct {
	SchemaVersion uint16 `json:"schema_version"`
}

func NewVersioned() Versioned { return Versioned{SchemaVersion: SchemaVersion} }

// EBSProvider is the block-storage controller contract consumed by Spinifex.
// Its operation and idempotency shape follows the CSI controller service, but
// the transport remains NATS request/reply rather than CSI's gRPC transport.
type EBSProvider interface {
	GetCapabilities(context.Context, GetCapabilitiesRequest) (*GetCapabilitiesResponse, error)
	CreateVolume(context.Context, CreateVolumeRequest) (*Volume, error)
	GetVolume(context.Context, GetVolumeRequest) (*Volume, error)
	ExpandVolume(context.Context, ExpandVolumeRequest) (*Volume, error)
	DeleteVolume(context.Context, DeleteVolumeRequest) error
	CreateSnapshot(context.Context, CreateSnapshotRequest) (*Snapshot, error)
	DeleteSnapshot(context.Context, DeleteSnapshotRequest) error
	PublishVolume(context.Context, PublishVolumeRequest) (*PublishedVolume, error)
	UnpublishVolume(context.Context, UnpublishVolumeRequest) error
}

// Provider is retained as a concise alias for implementation code. New
// consumers should name the dependency EBSProvider at composition boundaries.
type Provider = EBSProvider

// Capabilities advertises optional provider behavior. Callers must branch on
// these values instead of assuming all implementations behave like viperblock.
type Capabilities struct {
	CopyOnWriteClone        bool `json:"copy_on_write_clone"`
	OnlineExpansion         bool `json:"online_expansion"`
	SparseExtentReporting   bool `json:"sparse_extent_reporting"`
	CrashConsistentSnapshot bool `json:"crash_consistent_snapshot"`
	VolumeSeeding           bool `json:"volume_seeding"`
}

type GetCapabilitiesRequest struct{ Versioned }

type GetCapabilitiesResponse struct {
	Versioned

	Capabilities Capabilities   `json:"capabilities"`
	Error        *ProviderError `json:"error,omitempty"`
}

// CapacityRange mirrors CSI's capacity-range semantics. RequiredBytes is the
// minimum acceptable capacity; LimitBytes, when non-zero, is the maximum.
type CapacityRange struct {
	RequiredBytes int64 `json:"required_bytes"`
	LimitBytes    int64 `json:"limit_bytes,omitempty"`
}

type VolumeState string

const (
	VolumeStateAvailable VolumeState = "available"
	VolumeStateInUse     VolumeState = "in-use"
)

// Volume contains only facts the control plane can rely on across providers.
// Handle and ProviderData are opaque and must be passed back uninterpreted.
type Volume struct {
	ID               string          `json:"id"`
	CapacityBytes    int64           `json:"capacity_bytes"`
	State            VolumeState     `json:"state"`
	Handle           string          `json:"handle"`
	AvailabilityZone string          `json:"availability_zone,omitempty"`
	ProviderData     json.RawMessage `json:"provider_data,omitempty"`
}

// MaxSeedBytes bounds CreateVolumeRequest.SeedData. JSON encodes a []byte as
// base64, inflating it by 4/3, so this leaves the encoded request comfortably
// inside the 1MB NATS max_payload the cluster runs with.
const MaxSeedBytes = 640 * 1024

type CreateVolumeRequest struct {
	Versioned

	VolumeID         string          `json:"volume_id"`
	CapacityRange    CapacityRange   `json:"capacity_range"`
	AvailabilityZone string          `json:"availability_zone,omitempty"`
	SourceSnapshotID string          `json:"source_snapshot_id,omitempty"`
	SourceVolumeID   string          `json:"source_volume_id,omitempty"`
	Parameters       json.RawMessage `json:"parameters,omitempty"`

	// SeedData is written at offset 0 of a newly created volume. It exists so
	// the caller can supply host-local bytes, such as a firmware VARS template
	// whose layout must match the launching node, without shipping a path.
	SeedData []byte `json:"seed_data,omitempty"`
}

// ValidateSeedData rejects a seed the NATS transport cannot carry, so an
// oversized firmware template fails with an actionable error at the caller
// rather than as a truncated or refused publish.
func ValidateSeedData(seed []byte) error {
	if len(seed) > MaxSeedBytes {
		return fmt.Errorf("%w: seed data is %d bytes, limit is %d", ErrInvalidArgument, len(seed), MaxSeedBytes)
	}
	return nil
}

type CreateVolumeResponse struct {
	Versioned

	Volume *Volume        `json:"volume,omitempty"`
	Error  *ProviderError `json:"error,omitempty"`
}

type GetVolumeRequest struct {
	Versioned

	VolumeID string `json:"volume_id"`
	Handle   string `json:"handle,omitempty"`
}

type GetVolumeResponse struct {
	Versioned

	Volume *Volume        `json:"volume,omitempty"`
	Error  *ProviderError `json:"error,omitempty"`
}

type ExpandVolumeRequest struct {
	Versioned

	VolumeID      string        `json:"volume_id"`
	Handle        string        `json:"handle,omitempty"`
	CapacityRange CapacityRange `json:"capacity_range"`
}

type ExpandVolumeResponse struct {
	Versioned

	Volume *Volume        `json:"volume,omitempty"`
	Error  *ProviderError `json:"error,omitempty"`
}

type DeleteVolumeRequest struct {
	Versioned

	VolumeID string `json:"volume_id"`
	Handle   string `json:"handle,omitempty"`
}

type DeleteVolumeResponse struct {
	Versioned

	Error *ProviderError `json:"error,omitempty"`
}

type SnapshotState string

const (
	SnapshotStatePending   SnapshotState = "pending"
	SnapshotStateCompleted SnapshotState = "completed"
	SnapshotStateError     SnapshotState = "error"
)

type Snapshot struct {
	ID             string        `json:"id"`
	SourceVolumeID string        `json:"source_volume_id"`
	SizeBytes      int64         `json:"size_bytes"`
	CreatedAt      time.Time     `json:"created_at"`
	State          SnapshotState `json:"state"`
	Handle         string        `json:"handle"`
}

type CreateSnapshotRequest struct {
	Versioned

	SnapshotID   string `json:"snapshot_id"`
	VolumeID     string `json:"volume_id"`
	VolumeHandle string `json:"volume_handle,omitempty"`
}

// CreateSnapshotResponse is both the immediate accepted response and the
// completion event. Pending responses carry a completion subject; completed
// responses additionally carry the provider-neutral Snapshot.
type CreateSnapshotResponse struct {
	Versioned

	OperationID       string         `json:"operation_id"`
	CompletionSubject string         `json:"completion_subject,omitempty"`
	Snapshot          *Snapshot      `json:"snapshot,omitempty"`
	Error             *ProviderError `json:"error,omitempty"`
}

type DeleteSnapshotRequest struct {
	Versioned

	SnapshotID string `json:"snapshot_id"`
	Handle     string `json:"handle,omitempty"`
}

type DeleteSnapshotResponse struct {
	Versioned

	Error *ProviderError `json:"error,omitempty"`
}

type PublishVolumeRequest struct {
	Versioned

	VolumeID string `json:"volume_id"`
	Handle   string `json:"handle,omitempty"`
	NodeID   string `json:"node_id"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

type PublishedVolume struct {
	VolumeID string `json:"volume_id"`
	NodeID   string `json:"node_id"`
	NBDURI   string `json:"nbd_uri"`
}

type PublishVolumeResponse struct {
	Versioned

	Published *PublishedVolume `json:"published,omitempty"`
	Error     *ProviderError   `json:"error,omitempty"`
}

type UnpublishVolumeRequest struct {
	Versioned

	VolumeID string `json:"volume_id"`
	Handle   string `json:"handle,omitempty"`
	NodeID   string `json:"node_id"`
}

type UnpublishVolumeResponse struct {
	Versioned

	Error *ProviderError `json:"error,omitempty"`
}
