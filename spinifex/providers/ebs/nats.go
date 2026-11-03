package ebsprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

// subjectPrefix is common to every subject in the contract, including the
// per-node mount/unmount subjects that have no constant of their own.
const subjectPrefix = "ebs.provider.v1."

const (
	CapabilitiesSubject   = "ebs.provider.v1.capabilities"
	CreateVolumeSubject   = "ebs.provider.v1.volume.create"
	GetVolumeSubject      = "ebs.provider.v1.volume.describe"
	ListVolumesSubject    = "ebs.provider.v1.volume.list"
	ExpandVolumeSubject   = "ebs.provider.v1.volume.expand"
	DeleteVolumeSubject   = "ebs.provider.v1.volume.delete"
	DeleteSnapshotSubject = "ebs.provider.v1.snapshot.delete"
	CopySnapshotSubject   = "ebs.provider.v1.snapshot.copy"
	ListSnapshotsSubject  = "ebs.provider.v1.snapshot.list"

	// SnapshotCreateSubjectPrefix is the wildcard prefix servers subscribe to
	// (SnapshotCreateSubjectPrefix + "*") to catch every per-volume create
	// subject SnapshotSubject builds.
	SnapshotCreateSubjectPrefix = "ebs.provider.v1.snapshot.create."
)

const defaultRequestTimeout = 30 * time.Second

// dataMovementTimeout bounds the subjects whose duration is a function of the
// volume rather than of the control plane. It must exceed viperblockd's 120s
// kill grace plus a seal allowance, or an unpublish that uses the whole grace
// could never be seen to succeed.
const dataMovementTimeout = 180 * time.Second

// dataMovementVerbs names every operation that references or moves real data,
// however its subject addresses it. Everything absent answers from state
// already held and keeps the caller's timeout, so a new verb stays fast.
var dataMovementVerbs = map[string]bool{
	verbVolumeCreate:   true,
	verbVolumeExpand:   true,
	verbVolumeDelete:   true,
	verbSnapshotCreate: true,
	verbSnapshotCopy:   true,
	verbSnapshotDelete: true,
	// Unpublish seals local state back to the backend, which takes as long as
	// the volume has unsealed writes.
	verbVolumeUnpublish: true,
}

// timeoutFor sizes a request to the work its subject names.
func (p *NATSProvider) timeoutFor(subject string) time.Duration {
	if movesData(subject) {
		return max(p.requestTimeout, dataMovementTimeout)
	}
	return p.requestTimeout
}

func movesData(subject string) bool {
	return dataMovementVerbs[parseSubject(subject).verb]
}

// SnapshotSubject addresses a create by source volume so the owning node can
// serve it. The create verb is its own token: a bare volume ID here would
// collide with DeleteSnapshotSubject and make the wildcard unsubscribable.
func SnapshotSubject(volumeID string) (string, error) {
	if err := validateSubjectToken(volumeID); err != nil {
		return "", err
	}
	return SnapshotCreateSubjectPrefix + volumeID, nil
}

// SnapshotCompletionSubject returns the subject the provider publishes a
// snapshot's final CreateSnapshotResponse on once its data is captured. It
// fails with ErrInvalidArgument if snapshotID is not a valid subject token.
func SnapshotCompletionSubject(snapshotID string) (string, error) {
	if err := validateSubjectToken(snapshotID); err != nil {
		return "", err
	}
	return "ebs.provider.v1.snapshot.response." + snapshotID, nil
}

// Last tokens of the per-node subjects: ebs.provider.v1.<node>.<verb>.
const (
	publishVerb     = "mount"
	unpublishVerb   = "unmount"
	unsealedVerb    = "unsealed"
	fenceNoticeVerb = "fenced"
)

// nodeSubjectWildcard is the subject matching verb on every node, for a server
// that stands in for every node's provider.
func nodeSubjectWildcard(verb string) string { return subjectPrefix + "*." + verb }

// Wildcards over every node for each per-node subject.
var (
	PublishSubjectWildcard        = nodeSubjectWildcard(publishVerb)
	UnpublishSubjectWildcard      = nodeSubjectWildcard(unpublishVerb)
	UnsealedWritesSubjectWildcard = nodeSubjectWildcard(unsealedVerb)
)

func nodeSubject(nodeID, verb string) (string, error) {
	if err := validateSubjectToken(nodeID); err != nil {
		return "", err
	}
	return subjectPrefix + nodeID + "." + verb, nil
}

// PublishSubject returns nodeID's per-node mount subject
// (ebs.provider.v1.<node>.mount), which serves PublishVolume on that node.
func PublishSubject(nodeID string) (string, error) {
	return nodeSubject(nodeID, publishVerb)
}

// UnpublishSubject returns nodeID's per-node unmount subject
// (ebs.provider.v1.<node>.unmount), which serves UnpublishVolume on that node.
func UnpublishSubject(nodeID string) (string, error) {
	return nodeSubject(nodeID, unpublishVerb)
}

// UnsealedWritesSubject returns nodeID's per-node subject
// (ebs.provider.v1.<node>.unsealed), which answers whether that node holds
// unsealed writes for a set of volumes.
func UnsealedWritesSubject(nodeID string) (string, error) {
	return nodeSubject(nodeID, unsealedVerb)
}

// FenceNoticeSubject returns nodeID's fence-notice subject
// (ebs.provider.v1.<node>.fenced), on which a provider that revoked a live
// export there sends a FenceNotice for that node's daemon to acknowledge.
func FenceNoticeSubject(nodeID string) (string, error) {
	return nodeSubject(nodeID, fenceNoticeVerb)
}

// ownerSubjectPrefix is common to every owner-routed subject OwnerSubject
// builds: ebs.provider.v1.owner.{volumeID}.{verb}.
const ownerSubjectPrefix = subjectPrefix + "owner."

// Owner verbs are a closed set: only these four operations have an
// owner-routed variant, so callers use the named wrapper functions below
// instead of a string literal at the call site.
const (
	verbSnapshotCreate = "snapshot.create"
	verbSnapshotCopy   = "snapshot.copy"
	verbVolumeExpand   = "volume.expand"
	verbVolumeDescribe = "volume.describe"
)

// OwnerSubject addresses volumeID's mounting node directly, bypassing the
// spinifex-workers queue group that would otherwise deliver the request to
// a random node. verb must be one of the unexported verb constants above.
func OwnerSubject(volumeID, verb string) (string, error) {
	if err := validateSubjectToken(volumeID); err != nil {
		return "", err
	}
	return ownerSubjectPrefix + volumeID + "." + verb, nil
}

// SnapshotCreateOwnerSubject returns the owner-routed CreateSnapshot subject
// for volumeID; see OwnerSubject.
func SnapshotCreateOwnerSubject(volumeID string) (string, error) {
	return OwnerSubject(volumeID, verbSnapshotCreate)
}

// SnapshotCopyOwnerSubject returns the owner-routed CopySnapshot subject for
// volumeID, the snapshot's source volume; see OwnerSubject.
func SnapshotCopyOwnerSubject(volumeID string) (string, error) {
	return OwnerSubject(volumeID, verbSnapshotCopy)
}

// ExpandVolumeOwnerSubject returns the owner-routed ExpandVolume subject for
// volumeID; see OwnerSubject.
func ExpandVolumeOwnerSubject(volumeID string) (string, error) {
	return OwnerSubject(volumeID, verbVolumeExpand)
}

// GetVolumeOwnerSubject returns the owner-routed GetVolume subject for
// volumeID; see OwnerSubject.
func GetVolumeOwnerSubject(volumeID string) (string, error) {
	return OwnerSubject(volumeID, verbVolumeDescribe)
}

// ParseOwnerSubject recovers the volumeID and verb OwnerSubject encoded into
// subject. volumeID cannot itself contain '.' (validateSubjectToken enforces
// this), so the first '.' after ownerSubjectPrefix is always the
// volumeID/verb boundary, even though every verb also contains one.
func ParseOwnerSubject(subject string) (volumeID, verb string, ok bool) {
	rest, found := strings.CutPrefix(subject, ownerSubjectPrefix)
	if !found {
		return "", "", false
	}
	volumeID, verb, found = strings.Cut(rest, ".")
	if !found || volumeID == "" || verb == "" {
		return "", "", false
	}
	return volumeID, verb, true
}

func validateSubjectToken(value string) error {
	if value == "" || strings.ContainsAny(value, ".*>") {
		return fmt.Errorf("%w: invalid NATS subject token %q", ErrInvalidArgument, value)
	}
	return nil
}

// A publish answered without an error but without a usable export. The
// provider may have started the export anyway, so a caller must not assume
// nothing is left to tear down.
var (
	ErrNoPublishedVolume = errors.New("returned no published volume")
	ErrInvalidExport     = errors.New("returned an invalid export")
)

// NATSProvider drives a provider daemon through the versioned ebs.* contract.
type NATSProvider struct {
	conn           *nats.Conn
	requestTimeout time.Duration
}

var (
	_ EBSProvider           = (*NATSProvider)(nil)
	_ UnsealedWritesChecker = (*NATSProvider)(nil)
)

// NewNATSProvider returns a provider client over conn. A non-positive
// requestTimeout takes the 30s default; data-moving subjects get at least 180s.
func NewNATSProvider(conn *nats.Conn, requestTimeout time.Duration) *NATSProvider {
	if requestTimeout <= 0 {
		requestTimeout = defaultRequestTimeout
	}
	return &NATSProvider{conn: conn, requestTimeout: requestTimeout}
}

func (p *NATSProvider) GetCapabilities(ctx context.Context, req GetCapabilitiesRequest) (*GetCapabilitiesResponse, error) {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	var response GetCapabilitiesResponse
	if err := p.request(ctx, CapabilitiesSubject, req, &response); err != nil {
		return nil, err
	}
	if err := responseError(response.SchemaVersion, response.Error); err != nil {
		return nil, err
	}
	return &response, nil
}

func (p *NATSProvider) CreateVolume(ctx context.Context, req CreateVolumeRequest) (*Volume, error) {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	if err := ValidateSeedData(req.SeedData); err != nil {
		return nil, err
	}
	var response CreateVolumeResponse
	if err := p.request(ctx, CreateVolumeSubject, req, &response); err != nil {
		return nil, err
	}
	if err := responseError(response.SchemaVersion, response.Error); err != nil {
		return nil, err
	}
	if response.Volume == nil {
		return nil, fmt.Errorf("ebs.create returned no volume")
	}
	return response.Volume, nil
}

func (p *NATSProvider) GetVolume(ctx context.Context, req GetVolumeRequest) (*Volume, error) {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	ownerSubject, err := GetVolumeOwnerSubject(req.VolumeID)
	if err != nil {
		return nil, err
	}
	var response GetVolumeResponse
	if err := p.requestOwnerFirst(ctx, ownerSubject, GetVolumeSubject, req, &response); err != nil {
		return nil, err
	}
	if err := responseError(response.SchemaVersion, response.Error); err != nil {
		return nil, err
	}
	if response.Volume == nil {
		return nil, fmt.Errorf("ebs.describe returned no volume")
	}
	return response.Volume, nil
}

// ListVolumes asks the provider what it holds. It is not owner-routed: the
// question is about the whole provider, not one volume, so it goes to the
// queue group rather than to any single volume's mounting node.
func (p *NATSProvider) ListVolumes(ctx context.Context, req ListVolumesRequest) (*ListVolumesResponse, error) {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	var response ListVolumesResponse
	if err := p.request(ctx, ListVolumesSubject, req, &response); err != nil {
		return nil, err
	}
	if err := responseError(response.SchemaVersion, response.Error); err != nil {
		return nil, err
	}
	return &response, nil
}

func (p *NATSProvider) ExpandVolume(ctx context.Context, req ExpandVolumeRequest) (*Volume, error) {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	ownerSubject, err := ExpandVolumeOwnerSubject(req.VolumeID)
	if err != nil {
		return nil, err
	}
	var response ExpandVolumeResponse
	if err := p.requestOwnerFirst(ctx, ownerSubject, ExpandVolumeSubject, req, &response); err != nil {
		return nil, err
	}
	if err := responseError(response.SchemaVersion, response.Error); err != nil {
		return nil, err
	}
	if response.Volume == nil {
		return nil, fmt.Errorf("ebs.expand returned no volume")
	}
	return response.Volume, nil
}

func (p *NATSProvider) DeleteVolume(ctx context.Context, req DeleteVolumeRequest) error {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return err
	}
	var response DeleteVolumeResponse
	if err := p.request(ctx, DeleteVolumeSubject, req, &response); err != nil {
		return err
	}
	return responseError(response.SchemaVersion, response.Error)
}

func (p *NATSProvider) CreateSnapshot(ctx context.Context, req CreateSnapshotRequest) (*Snapshot, error) {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	requestSubject, err := SnapshotSubject(req.VolumeID)
	if err != nil {
		return nil, err
	}
	ownerSubject, err := SnapshotCreateOwnerSubject(req.VolumeID)
	if err != nil {
		return nil, err
	}
	completionSubject, err := SnapshotCompletionSubject(req.SnapshotID)
	if err != nil {
		return nil, err
	}
	if p.conn == nil || !p.conn.IsConnected() {
		return nil, nats.ErrConnectionClosed
	}

	completionSub, err := p.conn.SubscribeSync(completionSubject)
	if err != nil {
		return nil, fmt.Errorf("subscribe to snapshot completion: %w", err)
	}
	defer completionSub.Unsubscribe()
	if err := p.conn.Flush(); err != nil {
		return nil, fmt.Errorf("flush snapshot completion subscription: %w", err)
	}

	var accepted CreateSnapshotResponse
	if err := p.requestOwnerFirst(ctx, ownerSubject, requestSubject, req, &accepted); err != nil {
		return nil, err
	}
	if err := responseError(accepted.SchemaVersion, accepted.Error); err != nil {
		return nil, err
	}
	if accepted.Snapshot != nil && accepted.Snapshot.State != SnapshotStatePending {
		return accepted.Snapshot, nil
	}
	if accepted.OperationID == "" {
		return nil, fmt.Errorf("%s returned no operation ID", requestSubject)
	}
	if accepted.CompletionSubject != "" && accepted.CompletionSubject != completionSubject {
		return nil, fmt.Errorf("snapshot completion subject mismatch: got %q, want %q", accepted.CompletionSubject, completionSubject)
	}

	waitCtx, waitSpan := startCompletionSpan(ctx, verbSnapshotCreate, completionSubject)
	defer waitSpan.End()
	msg, err := completionSub.NextMsgWithContext(waitCtx)
	if err != nil {
		RecordSpanError(waitSpan, err)
		return nil, fmt.Errorf("wait for snapshot %s completion: %w", req.SnapshotID, err)
	}
	RecordResponseError(waitSpan, msg.Data)
	var completed CreateSnapshotResponse
	if err := json.Unmarshal(msg.Data, &completed); err != nil {
		return nil, fmt.Errorf("decode snapshot completion: %w", err)
	}
	if err := responseError(completed.SchemaVersion, completed.Error); err != nil {
		return nil, err
	}
	if completed.OperationID != accepted.OperationID {
		return nil, fmt.Errorf("snapshot operation mismatch: got %q, want %q", completed.OperationID, accepted.OperationID)
	}
	if completed.Snapshot == nil || completed.Snapshot.State != SnapshotStateCompleted {
		return nil, fmt.Errorf("snapshot %s completion returned no completed snapshot", req.SnapshotID)
	}
	return completed.Snapshot, nil
}

func (p *NATSProvider) DeleteSnapshot(ctx context.Context, req DeleteSnapshotRequest) error {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return err
	}
	var response DeleteSnapshotResponse
	if err := p.request(ctx, DeleteSnapshotSubject, req, &response); err != nil {
		return err
	}
	return responseError(response.SchemaVersion, response.Error)
}

// CopySnapshot is a plain synchronous request/reply, unlike CreateSnapshot's
// accept-then-publish pattern: duplicating a snapshot's metadata is a couple
// of small object writes with no flush/upload step slow enough to warrant a
// completion subject.
func (p *NATSProvider) CopySnapshot(ctx context.Context, req CopySnapshotRequest) (*Snapshot, error) {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	ownerSubject, err := SnapshotCopyOwnerSubject(req.VolumeID)
	if err != nil {
		return nil, err
	}
	var response CopySnapshotResponse
	if err := p.requestOwnerFirst(ctx, ownerSubject, CopySnapshotSubject, req, &response); err != nil {
		return nil, err
	}
	if err := responseError(response.SchemaVersion, response.Error); err != nil {
		return nil, err
	}
	if response.Snapshot == nil {
		return nil, fmt.Errorf("ebs.snapshot.copy returned no snapshot")
	}
	return response.Snapshot, nil
}

// ListSnapshots asks the provider what it holds. Like ListVolumes it is not
// owner-routed: the question is about the whole provider, not one snapshot.
func (p *NATSProvider) ListSnapshots(ctx context.Context, req ListSnapshotsRequest) (*ListSnapshotsResponse, error) {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	var response ListSnapshotsResponse
	if err := p.request(ctx, ListSnapshotsSubject, req, &response); err != nil {
		return nil, err
	}
	if err := responseError(response.SchemaVersion, response.Error); err != nil {
		return nil, err
	}
	return &response, nil
}

func (p *NATSProvider) PublishVolume(ctx context.Context, req PublishVolumeRequest) (*PublishedVolume, error) {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	subject, err := PublishSubject(req.NodeID)
	if err != nil {
		return nil, err
	}
	if req.SchemaVersion == LegacySchemaVersion {
		var response PublishVolumeResponseV1
		if err := p.request(ctx, subject, req, &response); err != nil {
			return nil, err
		}
		if err := responseError(response.SchemaVersion, response.Error); err != nil {
			return nil, err
		}
		if response.Published == nil {
			return nil, fmt.Errorf("%s %w", subject, ErrNoPublishedVolume)
		}
		published := PublishedVolumeFromV1(response.Published)
		if err := published.Export.Validate(); err != nil {
			return nil, fmt.Errorf("%s %w: %w", subject, ErrInvalidExport, err)
		}
		return published, nil
	}
	var response PublishVolumeResponse
	if err := p.request(ctx, subject, req, &response); err != nil {
		return nil, err
	}
	if err := responseError(response.SchemaVersion, response.Error); err != nil {
		return nil, err
	}
	if response.Published == nil {
		return nil, fmt.Errorf("%s %w", subject, ErrNoPublishedVolume)
	}
	if err := response.Published.Export.Validate(); err != nil {
		return nil, fmt.Errorf("%s %w: %w", subject, ErrInvalidExport, err)
	}
	return response.Published, nil
}

func (p *NATSProvider) UnpublishVolume(ctx context.Context, req UnpublishVolumeRequest) (*UnpublishedVolume, error) {
	if err := CheckUnpublishVersion(req); err != nil {
		return nil, err
	}
	subject, err := UnpublishSubject(req.NodeID)
	if err != nil {
		return nil, err
	}
	var response UnpublishVolumeResponse
	if err := p.request(ctx, subject, req, &response); err != nil {
		return nil, err
	}
	if err := responseError(response.SchemaVersion, response.Error); err != nil {
		return nil, err
	}
	return &UnpublishedVolume{TornDown: response.TornDown}, nil
}

// HoldsUnsealedWrites returns the subset of volumeIDs for which nodeID's
// provider holds writes the backend may not have. An error means the node could
// not say, which a caller deciding whether to start elsewhere must not read as no.
func (p *NATSProvider) HoldsUnsealedWrites(ctx context.Context, nodeID string, volumeIDs []string) ([]string, error) {
	subject, err := UnsealedWritesSubject(nodeID)
	if err != nil {
		return nil, err
	}
	req := HoldsUnsealedWritesRequest{Versioned: NewVersioned(), NodeID: nodeID, VolumeIDs: volumeIDs}
	var response HoldsUnsealedWritesResponse
	if err := p.request(ctx, subject, req, &response); err != nil {
		return nil, err
	}
	if err := responseError(response.SchemaVersion, response.Error); err != nil {
		return nil, err
	}
	return response.Held, nil
}

func (p *NATSProvider) request(ctx context.Context, subject string, input, output any) error {
	// Only a closed connection refuses: one that is reconnecting buffers the
	// request and sends it once the connection is back.
	if p.conn == nil || p.conn.IsClosed() {
		return nats.ErrConnectionClosed
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("encode %s request: %w", subject, err)
	}

	request := &nats.Msg{Subject: subject, Data: payload, Header: nats.Header{}}
	ctx, span := startClientSpan(ctx, subject, request.Header)
	defer span.End()

	requestCtx, cancel := context.WithTimeout(ctx, p.timeoutFor(subject))
	defer cancel()
	msg, err := p.conn.RequestMsgWithContext(requestCtx, request)
	if err != nil {
		RecordSpanError(span, err)
		return fmt.Errorf("request %s: %w", subject, err)
	}
	if err := json.Unmarshal(msg.Data, output); err != nil {
		RecordSpanError(span, err)
		return fmt.Errorf("decode %s response: %w", subject, err)
	}
	RecordResponseError(span, msg.Data)
	return nil
}

// requestOwnerFirst tries ownerSubject, falling back to queueSubject only on
// nats.ErrNoResponders, which means nothing is mounted. A timeout must never
// fall back: a still-running owner would have the operation run twice.
func (p *NATSProvider) requestOwnerFirst(ctx context.Context, ownerSubject, queueSubject string, input, output any) error {
	err := p.request(ctx, ownerSubject, input, output)
	if err == nil || !errors.Is(err, nats.ErrNoResponders) {
		return err
	}
	return p.request(ctx, queueSubject, input, output)
}

func responseError(version uint16, providerErr *ProviderError) error {
	if err := checkVersion(version); err != nil {
		return err
	}
	if providerErr != nil {
		return providerErr
	}
	return nil
}
