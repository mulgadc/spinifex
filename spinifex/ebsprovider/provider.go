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
	ListVolumes(context.Context, ListVolumesRequest) (*ListVolumesResponse, error)
	ExpandVolume(context.Context, ExpandVolumeRequest) (*Volume, error)
	DeleteVolume(context.Context, DeleteVolumeRequest) error
	CreateSnapshot(context.Context, CreateSnapshotRequest) (*Snapshot, error)
	DeleteSnapshot(context.Context, DeleteSnapshotRequest) error
	CopySnapshot(context.Context, CopySnapshotRequest) (*Snapshot, error)
	ListSnapshots(context.Context, ListSnapshotsRequest) (*ListSnapshotsResponse, error)
	PublishVolume(context.Context, PublishVolumeRequest) (*PublishedVolume, error)
	UnpublishVolume(context.Context, UnpublishVolumeRequest) error
}

// Provider is retained as a concise alias for implementation code. New
// consumers should name the dependency EBSProvider at composition boundaries.
type Provider = EBSProvider

// Capabilities advertises optional provider behavior. Callers must branch on
// these values instead of assuming all implementations behave like viperblock.
//
// Every field must name behaviour some request in this interface can ask for.
// A capability no verb reaches tells a caller a feature is available and then
// gives them no way to use it.
type Capabilities struct {
	OnlineExpansion         bool `json:"online_expansion"`
	SparseExtentReporting   bool `json:"sparse_extent_reporting"`
	CrashConsistentSnapshot bool `json:"crash_consistent_snapshot"`
	VolumeSeeding           bool `json:"volume_seeding"`

	// ReadOnlyPublish advertises that PublishVolume honours ReadOnly by
	// exporting the volume read-only. A provider leaving this false must
	// refuse a ReadOnly request, never hand back a writable export instead.
	ReadOnlyPublish bool `json:"read_only_publish"`

	// VolumeEnumeration advertises that ListVolumes answers with what the
	// provider actually holds. Without it the control plane's only index of
	// volumes is its own metadata, so a lost document strands the blocks.
	VolumeEnumeration bool `json:"volume_enumeration"`

	// SnapshotEnumeration advertises that ListSnapshots answers with what the
	// provider actually holds. It is the same exposure VolumeEnumeration
	// closes, for snapshots, and is advertised separately because a provider
	// can enumerate one without the other.
	SnapshotEnumeration bool `json:"snapshot_enumeration"`

	// OwnerRouting advertises that CreateSnapshot, CopySnapshot, ExpandVolume
	// and GetVolume are answered directly by a volume's mounting node over
	// its OwnerSubject when one exists, instead of always fanning out to the
	// spinifex-workers queue group.
	OwnerRouting bool `json:"owner_routing"`

	// Exclusion states how strongly this provider prevents two writers from
	// holding one volume at once. It is not optional behaviour a caller may
	// ignore: a caller that assumes cluster-wide exclusion from a provider
	// that only excludes within a node will corrupt the volume.
	Exclusion ExclusionSemantics `json:"exclusion"`
}

// ExclusionScope names how far a provider's single-writer guarantee reaches.
type ExclusionScope string

const (
	// ExclusionScopeNone means nothing prevents two writers. Two opens of one
	// volume both succeed and both write.
	ExclusionScopeNone ExclusionScope = "none"

	// ExclusionScopeNode means a second writer is refused on the same node and
	// nowhere else. A different node opening the same backing store is not
	// seen, so this is a guarantee about a process, not about a volume.
	ExclusionScopeNode ExclusionScope = "node"

	// ExclusionScopeCluster means a second writer is refused wherever it runs,
	// because the claim is held somewhere both nodes must consult.
	ExclusionScopeCluster ExclusionScope = "cluster"
)

// ExclusionSemantics is the single-writer guarantee, stated so a caller can
// tell what it actually gets. Three questions have distinct answers and the
// contract used to conflate them: how far exclusion reaches, whether a dead
// owner's claim ever clears, and whether a writer that lost its claim is
// actually stopped.
type ExclusionSemantics struct {
	Scope ExclusionScope `json:"scope"`

	// ClaimTTLSeconds is how long a claim outlives the writer holding it.
	// Zero means it never expires: a node that dies holding one strands the
	// volume until an operator clears it. Meaningless when Scope is none.
	ClaimTTLSeconds int `json:"claim_ttl_seconds,omitempty"`

	// FencesLostClaim reports whether a writer whose claim lapsed is stopped
	// from writing, rather than merely finding out. False is the dangerous
	// case and the reason this is not a bool on its own: exclusion refuses
	// the *second* opener, fencing stops the *first* one. Without fencing, a
	// partitioned node whose claim expired keeps writing, and the claim
	// expiring is precisely what lets a second writer in beside it.
	FencesLostClaim bool `json:"fences_lost_claim"`
}

// SingleWriter reports whether opening a volume twice anywhere is refused.
// Callers branch on this rather than comparing Scope, so adding a scope later
// does not need every call site found again.
func (e ExclusionSemantics) SingleWriter() bool {
	return e.Scope == ExclusionScopeCluster
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
	Parameters       json.RawMessage `json:"parameters,omitempty"`

	// SourceSnapshotVolumeID names the volume SourceSnapshotID was taken from,
	// matching that snapshot's Snapshot.SourceVolumeID. It is required with
	// SourceSnapshotID and meaningless without it: this is not a clone source.
	SourceSnapshotVolumeID string `json:"source_snapshot_volume_id,omitempty"`

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

// VolumeRef identifies a volume the provider holds. It is deliberately not a
// Volume: enumeration answers "what exists", and a provider must not have to
// open every volume's engine to answer that.
type VolumeRef struct {
	ID     string `json:"id"`
	Handle string `json:"handle,omitempty"`
}

// MaxListResults bounds one page of ListVolumes or ListSnapshots. The reply
// rides a NATS message, so the page must fit the cluster's max_payload; a
// VolumeRef or SnapshotRef encodes to well under 1KB, leaving headroom here.
const MaxListResults int32 = 1000

// ListVolumesRequest pages through the volumes a provider holds. A MaxResults
// above MaxListResults is clamped rather than refused: a caller asking for
// more than fits should get a page and a token, not an error.
type ListVolumesRequest struct {
	Versioned

	MaxResults    int32  `json:"max_results,omitempty"`
	StartingToken string `json:"starting_token,omitempty"`
}

// ListVolumesResponse carries one page. An empty NextToken means the last
// page; an empty Volumes list with no error means the provider holds none.
type ListVolumesResponse struct {
	Versioned

	Volumes   []VolumeRef    `json:"volumes,omitempty"`
	NextToken string         `json:"next_token,omitempty"`
	Error     *ProviderError `json:"error,omitempty"`
}

// PageSize is the number of refs a provider should return for this request.
func (r ListVolumesRequest) PageSize() int32 {
	if r.MaxResults <= 0 || r.MaxResults > MaxListResults {
		return MaxListResults
	}
	return r.MaxResults
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

// CopySnapshotRequest duplicates SourceSnapshotID under DestinationSnapshotID
// as a second, independently addressable snapshot over the same frozen data.
// VolumeID is the snapshot's own source volume, not a new source: the caller
// already knows it (it is the same VolumeID the original CreateSnapshotRequest
// carried), so the provider is not asked to resolve snapshot ownership itself.
type CopySnapshotRequest struct {
	Versioned

	SourceSnapshotID      string `json:"source_snapshot_id"`
	DestinationSnapshotID string `json:"destination_snapshot_id"`
	VolumeID              string `json:"volume_id"`
}

type CopySnapshotResponse struct {
	Versioned

	Snapshot *Snapshot      `json:"snapshot,omitempty"`
	Error    *ProviderError `json:"error,omitempty"`
}

// SnapshotRef identifies a snapshot the provider holds. Like VolumeRef it is
// deliberately not a Snapshot: enumeration answers "what exists", and reading
// a snapshot's size or state can cost far more than listing it.
//
// SourceVolumeID is carried because reconciliation needs it: a snapshot found
// with no metadata document is only actionable once its volume is known.
type SnapshotRef struct {
	ID             string `json:"id"`
	SourceVolumeID string `json:"source_volume_id,omitempty"`
	Handle         string `json:"handle,omitempty"`
}

// ListSnapshotsRequest pages through the snapshots a provider holds. It is
// paged and clamped exactly as ListVolumesRequest is.
type ListSnapshotsRequest struct {
	Versioned

	MaxResults    int32  `json:"max_results,omitempty"`
	StartingToken string `json:"starting_token,omitempty"`
}

// ListSnapshotsResponse carries one page. An empty NextToken means the last
// page; an empty Snapshots list with no error means the provider holds none.
type ListSnapshotsResponse struct {
	Versioned

	Snapshots []SnapshotRef  `json:"snapshots,omitempty"`
	NextToken string         `json:"next_token,omitempty"`
	Error     *ProviderError `json:"error,omitempty"`
}

// PageSize is the number of refs a provider should return for this request.
func (r ListSnapshotsRequest) PageSize() int32 {
	if r.MaxResults <= 0 || r.MaxResults > MaxListResults {
		return MaxListResults
	}
	return r.MaxResults
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
