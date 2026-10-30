// Package ebsprovider defines Spinifex's provider-neutral block-storage
// contract. Provider implementations must not expose their engine's Go types
// through this package; provider-specific settings and handles stay opaque.
package ebsprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

const (
	// LegacySchemaVersion is the oldest representation every supported release
	// understands. Ordinary callers use it until they have target-specific
	// evidence that the peer supports a newer representation.
	LegacySchemaVersion uint16 = 1

	// SchemaVersion is the newest wire-contract version understood by this
	// build. Every request and response carries a version so an unsupported
	// representation fails at the boundary rather than silently decoding it.
	SchemaVersion uint16 = 2
)

// Versioned is embedded in every request and response sent over NATS.
type Versioned struct {
	SchemaVersion uint16 `json:"schema_version"`
}

// NewVersioned returns the oldest representation guaranteed to interoperate
// during an N/N-1 rollout. Callers may use NewLatestVersioned only after a
// capability exchange proves the target understands SchemaVersion.
func NewVersioned() Versioned { return Versioned{SchemaVersion: LegacySchemaVersion} }

// NewLatestVersioned returns the newest representation this build understands.
// It must not be used merely because this binary is new: a separately released
// peer may still be serving LegacySchemaVersion.
func NewLatestVersioned() Versioned { return Versioned{SchemaVersion: SchemaVersion} }

// SupportsSchemaVersion reports whether this build can safely decode version.
func SupportsSchemaVersion(version uint16) bool {
	return version == LegacySchemaVersion || version == SchemaVersion
}

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

// GetCapabilitiesRequest asks the provider what it supports, over
// CapabilitiesSubject.
type GetCapabilitiesRequest struct{ Versioned }

// GetCapabilitiesResponse is the reply to GetCapabilitiesRequest. A non-nil
// Error means Capabilities is not set.
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

// VolumeState is a volume's attachment state, using the EC2 volume state names.
type VolumeState string

const (
	VolumeStateCreating  VolumeState = "creating"
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

// CreateVolumeRequest creates VolumeID, empty or from SourceSnapshotID, over
// CreateVolumeSubject. Parameters are provider-specific and passed through opaque.
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

// CreateVolumeResponse is the reply to CreateVolumeRequest: the created volume,
// or Error.
type CreateVolumeResponse struct {
	Versioned

	Volume *Volume        `json:"volume,omitempty"`
	Error  *ProviderError `json:"error,omitempty"`
}

// GetVolumeRequest describes one volume over GetVolumeSubject. Handle, when
// set, is the opaque handle the provider returned at create.
type GetVolumeRequest struct {
	Versioned

	VolumeID string `json:"volume_id"`
	Handle   string `json:"handle,omitempty"`
}

// GetVolumeResponse is the reply to GetVolumeRequest: the volume, or Error.
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

// ExpandVolumeRequest grows a volume to CapacityRange over ExpandVolumeSubject.
type ExpandVolumeRequest struct {
	Versioned

	VolumeID      string        `json:"volume_id"`
	Handle        string        `json:"handle,omitempty"`
	CapacityRange CapacityRange `json:"capacity_range"`
}

// ExpandVolumeResponse is the reply to ExpandVolumeRequest: the resized volume,
// or Error.
type ExpandVolumeResponse struct {
	Versioned

	Volume *Volume        `json:"volume,omitempty"`
	Error  *ProviderError `json:"error,omitempty"`
}

// DeleteVolumeRequest deletes a volume over DeleteVolumeSubject. Deleting a
// volume that does not exist succeeds; a published one fails as in use.
type DeleteVolumeRequest struct {
	Versioned

	VolumeID string `json:"volume_id"`
	Handle   string `json:"handle,omitempty"`
}

// DeleteVolumeResponse is the reply to DeleteVolumeRequest. A nil Error means
// the volume is gone.
type DeleteVolumeResponse struct {
	Versioned

	Error *ProviderError `json:"error,omitempty"`
}

// SnapshotState is a snapshot's lifecycle state, using the EC2 snapshot state
// names.
type SnapshotState string

const (
	SnapshotStatePending   SnapshotState = "pending"
	SnapshotStateCompleted SnapshotState = "completed"
	SnapshotStateError     SnapshotState = "error"
)

// Snapshot is the provider-neutral view of a snapshot. Handle is opaque and
// must be passed back uninterpreted.
type Snapshot struct {
	ID             string        `json:"id"`
	SourceVolumeID string        `json:"source_volume_id"`
	SizeBytes      int64         `json:"size_bytes"`
	CreatedAt      time.Time     `json:"created_at"`
	State          SnapshotState `json:"state"`
	Handle         string        `json:"handle"`
}

// CreateSnapshotRequest snapshots VolumeID as SnapshotID. It is sent to the
// volume's owner node first, then to SnapshotSubject.
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

// DeleteSnapshotRequest deletes a snapshot over DeleteSnapshotSubject.
type DeleteSnapshotRequest struct {
	Versioned

	SnapshotID string `json:"snapshot_id"`
	Handle     string `json:"handle,omitempty"`
}

// DeleteSnapshotResponse is the reply to DeleteSnapshotRequest. A nil Error
// means the snapshot is gone.
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

// CopySnapshotResponse is the reply to CopySnapshotRequest: the new snapshot,
// or Error.
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

// PublishVolumeRequest mounts a volume on NodeID so a VM there can attach it,
// over that node's PublishSubject.
type PublishVolumeRequest struct {
	Versioned

	VolumeID string `json:"volume_id"`
	Handle   string `json:"handle,omitempty"`
	NodeID   string `json:"node_id"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// ExportKind names how a published volume is reached on its node, and so
// which VolumeExport variant carries the details.
type ExportKind string

const (
	ExportKindNBD   ExportKind = "nbd"
	ExportKindBlock ExportKind = "block"
)

// VolumeExport is how a published volume is reached; exactly one of NBD and
// Block is set, and it is the one Kind names.
type VolumeExport struct {
	Kind  ExportKind   `json:"kind"`
	NBD   *NBDExport   `json:"nbd,omitempty"`
	Block *BlockExport `json:"block,omitempty"`
}

// NBDExport is a volume served over NBD: the URI on the node that QEMU
// connects to.
type NBDExport struct {
	NBDURI string `json:"nbd_uri"`
}

// BlockExport is a volume exposed as a host block device. SizeBytes is the
// size the guest sees, which may be smaller than the device itself.
type BlockExport struct {
	DevicePath string `json:"device_path"`
	SizeBytes  uint64 `json:"size_bytes"`
}

// Validate rejects an export a consumer could not attach: an unknown kind, a
// missing or extra variant, or a variant with an unusable field.
func (e VolumeExport) Validate() error {
	switch e.Kind {
	case ExportKindNBD:
		if e.NBD == nil {
			return errors.New("nbd export has no nbd details")
		}
		if e.Block != nil {
			return errors.New("nbd export also sets block details")
		}
		if e.NBD.NBDURI == "" {
			return errors.New("nbd export has an empty NBD URI")
		}
	case ExportKindBlock:
		if e.Block == nil {
			return errors.New("block export has no block details")
		}
		if e.NBD != nil {
			return errors.New("block export also sets nbd details")
		}
		if !filepath.IsAbs(e.Block.DevicePath) {
			return fmt.Errorf("block export device path %q is not absolute", e.Block.DevicePath)
		}
		if strings.Contains(e.Block.DevicePath, ",") {
			return fmt.Errorf("block export device path %q contains a comma", e.Block.DevicePath)
		}
		if e.Block.SizeBytes == 0 {
			return errors.New("block export has a zero size")
		}
	default:
		return fmt.Errorf("unknown export kind %q", e.Kind)
	}
	return nil
}

// PublishedVolume is where a published volume is served: the export on NodeID
// that QEMU attaches.
type PublishedVolume struct {
	VolumeID string       `json:"volume_id"`
	NodeID   string       `json:"node_id"`
	Export   VolumeExport `json:"export"`
}

// PublishedVolumeV1 is the schema-v1 publish result. It remains a public wire
// type so a v2 server can reply in the exact shape an N-1 client understands.
// New code uses PublishedVolume and VolumeExport instead.
type PublishedVolumeV1 struct {
	VolumeID string `json:"volume_id"`
	NodeID   string `json:"node_id"`
	NBDURI   string `json:"nbd_uri"`
}

// AsV1 converts an NBD export to the legacy response. A block export has no
// safe v1 projection: an old consumer would treat an empty NBD URI as usable.
func (p *PublishedVolume) AsV1() (*PublishedVolumeV1, error) {
	if p == nil {
		return nil, errors.New("published volume is nil")
	}
	if err := p.Export.Validate(); err != nil {
		return nil, err
	}
	if p.Export.Kind != ExportKindNBD {
		return nil, fmt.Errorf("%s export requires schema version %d", p.Export.Kind, SchemaVersion)
	}
	return &PublishedVolumeV1{
		VolumeID: p.VolumeID,
		NodeID:   p.NodeID,
		NBDURI:   p.Export.NBD.NBDURI,
	}, nil
}

// PublishedVolumeFromV1 converts the legacy NBD-only response to the current
// descriptor form. Validation remains with the transport boundary so callers
// receive the same error context for either wire representation.
func PublishedVolumeFromV1(p *PublishedVolumeV1) *PublishedVolume {
	if p == nil {
		return nil
	}
	return &PublishedVolume{
		VolumeID: p.VolumeID,
		NodeID:   p.NodeID,
		Export:   NewNBDExport(p.NBDURI),
	}
}

// NewNBDExport returns an nbd export for uri.
func NewNBDExport(uri string) VolumeExport {
	return VolumeExport{Kind: ExportKindNBD, NBD: &NBDExport{NBDURI: uri}}
}

// PublishVolumeResponse is the reply to PublishVolumeRequest: the mount, or
// Error.
type PublishVolumeResponse struct {
	Versioned

	Published *PublishedVolume `json:"published,omitempty"`
	Error     *ProviderError   `json:"error,omitempty"`
}

// PublishVolumeResponseV1 is the exact schema-v1 wire response retained for
// mixed-version operation. Its JSON shape is intentionally the pre-v2 shape.
type PublishVolumeResponseV1 struct {
	Versioned

	Published *PublishedVolumeV1 `json:"published,omitempty"`
	Error     *ProviderError     `json:"error,omitempty"`
}

// UnpublishVolumeRequest unmounts a volume from NodeID, over that node's
// UnpublishSubject.
type UnpublishVolumeRequest struct {
	Versioned

	VolumeID string `json:"volume_id"`
	Handle   string `json:"handle,omitempty"`
	NodeID   string `json:"node_id"`
}

// UnpublishVolumeResponse is the reply to UnpublishVolumeRequest. A nil Error
// means the volume is no longer published on the node.
type UnpublishVolumeResponse struct {
	Versioned

	Error *ProviderError `json:"error,omitempty"`
}

// UnsealedWritesChecker answers, for one node, which of a set of volumes it
// still holds writes for that the backend may not have. It is separate from
// EBSProvider because only the instance start path asks, and only of the node
// that last ran the instance.
type UnsealedWritesChecker interface {
	HoldsUnsealedWrites(ctx context.Context, nodeID string, volumeIDs []string) ([]string, error)
}

// HoldsUnsealedWritesRequest asks NodeID, over its UnsealedWritesSubject, which
// of VolumeIDs it holds unsealed writes for.
type HoldsUnsealedWritesRequest struct {
	Versioned

	NodeID    string   `json:"node_id"`
	VolumeIDs []string `json:"volume_ids"`
}

// HoldsUnsealedWritesResponse lists the requested volumes the node holds
// unsealed writes for. A nil Error with an empty Held means it holds none.
type HoldsUnsealedWritesResponse struct {
	Versioned

	Held  []string       `json:"held,omitempty"`
	Error *ProviderError `json:"error,omitempty"`
}
