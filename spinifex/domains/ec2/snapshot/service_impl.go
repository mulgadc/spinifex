package snapshot

import (
	"context"
	"errors"
	"fmt"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/tagmirror"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/bluebottle/pkg/safecast"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsfilters "github.com/mulgadc/spinifex/spinifex/foundation/aws/filters"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	awstags "github.com/mulgadc/spinifex/spinifex/foundation/aws/tags"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/migrate"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Ensure SnapshotServiceImpl implements SnapshotService.
var _ SnapshotService = (*SnapshotServiceImpl)(nil)

const (
	KVBucketVolumeSnapshots        = "spinifex-volume-snapshots"
	KVBucketVolumeSnapshotsVersion = 1
	KVBucketVolumeSnapshotsHistory = 10

	// snapshotRefCASAttempts bounds one index write's retry on a revision
	// conflict. Every snapshot of a volume shares one record, so simultaneous
	// creates on one volume contend once each; the default five sits below that.
	snapshotRefCASAttempts = 25
	snapshotCleanupTimeout = 5 * time.Second
)

// SnapshotServiceImpl implements SnapshotService with S3-backed storage.
type SnapshotServiceImpl struct {
	config   *config.Config
	store    objectstore.ObjectStore
	natsConn *nats.Conn
	snapRefs *kvstore.Store[[]string]
	mutex    sync.RWMutex
	metadata *ebsmetadata.Store
	provider ebsprovider.EBSProvider
	claims   *Claims

	// crashAt stops CreateSnapshot after a step, as a crash there would. Tests only.
	crashAt func(createStep) bool
	// reapAt interrupts the reaper at a step for snapshotID. Tests only.
	reapAt func(step reapStep, snapshotID string) bool
	// now is the reaper's clock for tombstones; nil is time.Now. Tests only.
	now func() time.Time
	// claimRetryDelay overrides the wait between claim attempts. Tests only.
	claimRetryDelay time.Duration
	// claimRefused observes each refused claim attempt of a writer. Tests only.
	claimRefused func(snapshotID string)
}

// clock is the time the reaper judges a tombstone's age by.
func (s *SnapshotServiceImpl) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// SetClaims wires the snapshot claims shared by CreateSnapshot and the
// pending-snapshot reaper. Without it they exclude only within this process.
func (s *SnapshotServiceImpl) SetClaims(claims *Claims) {
	s.claims = claims
}

// SetEBSProvider injects the provider boundary for snapshot operations.
// Legacy snapshot behavior remains until snapshot metadata is migrated.
func (s *SnapshotServiceImpl) SetEBSProvider(provider ebsprovider.EBSProvider) {
	s.provider = provider
}

// EBSProvider returns the injected provider boundary, or nil on the legacy
// embedded-engine path. Primarily for composition-root tests to observe wiring.
func (s *SnapshotServiceImpl) EBSProvider() ebsprovider.EBSProvider {
	return s.provider
}

// MetadataStore returns the control-plane metadata store. Primarily for
// composition-root tests to observe wiring.
func (s *SnapshotServiceImpl) MetadataStore() *ebsmetadata.Store {
	return s.metadata
}

// NewSnapshotServiceImplWithNATS creates a snapshot service with JetStream KV for volume-snapshot tracking.
func NewSnapshotServiceImplWithNATS(ctx context.Context, cfg *config.Config, natsConn *nats.Conn) (*SnapshotServiceImpl, jetstream.KeyValue, error) {
	store := objectstore.NewS3ObjectStoreFromConfig(
		cfg.Predastore.Host,
		cfg.Predastore.Region,
		cfg.Predastore.AccessKey,
		cfg.Predastore.SecretKey,
	)

	js, err := jetstream.New(natsConn)
	if err != nil {
		return nil, nil, fmt.Errorf("create JetStream: %w", err)
	}

	refs := kvstore.New[[]string](js, snapshotRefsConfig())
	// Opened eagerly: the handle is returned to the caller, which wires it into
	// the volume service, so it has to exist before this function returns.
	kv, err := refs.KV(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("create KV bucket %s: %w", KVBucketVolumeSnapshots, err)
	}

	slog.Info("Snapshot service initialized with JetStream KV", "bucket", KVBucketVolumeSnapshots)

	return &SnapshotServiceImpl{
		config:   cfg,
		store:    store,
		natsConn: natsConn,
		snapRefs: refs,
		metadata: ebsmetadata.NewStore(store, cfg.Predastore.Bucket),
		claims:   localClaims(),
	}, kv, nil
}

// NewSnapshotServiceImplWithStore creates a snapshot service with a custom ObjectStore (for testing).
// An optional snapshotKV can be provided for KV-backed volume-snapshot tracking.
func NewSnapshotServiceImplWithStore(cfg *config.Config, store objectstore.ObjectStore, natsConn *nats.Conn, snapshotKV ...jetstream.KeyValue) *SnapshotServiceImpl {
	svc := &SnapshotServiceImpl{
		config:   cfg,
		store:    store,
		natsConn: natsConn,
		metadata: ebsmetadata.NewStore(store, cfg.Predastore.Bucket),
		claims:   localClaims(),
	}
	if len(snapshotKV) > 0 {
		svc.snapRefs = kvstore.Over[[]string](nil, snapshotKV[0], snapshotRefsConfig())
	}
	return svc
}

// snapshotRefsConfig describes the volume-to-snapshot index bucket. The value
// is the list of snapshot IDs taken from one volume, keyed by volume ID.
func snapshotRefsConfig() kvstore.Config {
	return kvstore.Config{
		Name:     KVBucketVolumeSnapshots,
		History:  KVBucketVolumeSnapshotsHistory,
		Missing:  "volume snapshot KV bucket not initialized",
		Attempts: snapshotRefCASAttempts,
		OnOpen: func(ctx context.Context, kv jetstream.KeyValue) error {
			return migrate.DefaultRegistry.RunKV(ctx, KVBucketVolumeSnapshots, kv, KVBucketVolumeSnapshotsVersion)
		},
	}
}

// getSnapshotConfig reads the caller's own snapshot document. A snapshot
// outside the caller's prefix is absent, so ownership needs no second check,
// and so is one whose delete was accepted.
func (s *SnapshotServiceImpl) getSnapshotConfig(ctx context.Context, accountID, snapshotID string) (ebsmetadata.Snapshot, error) {
	cfg, err := s.metadata.GetSnapshot(ctx, accountID, snapshotID)
	if err != nil {
		if objectstore.IsNoSuchKeyError(err) {
			return ebsmetadata.Snapshot{}, errors.New(awserrors.ErrorInvalidSnapshotNotFound)
		}
		return ebsmetadata.Snapshot{}, err
	}
	if cfg.Hidden() {
		return ebsmetadata.Snapshot{}, errors.New(awserrors.ErrorInvalidSnapshotNotFound)
	}
	return cfg, nil
}

// snapshotConfigToEC2 converts a snapshot document to an EC2 response object.
func snapshotConfigToEC2(cfg ebsmetadata.Snapshot) *ec2.Snapshot {
	snapshot := &ec2.Snapshot{
		SnapshotId:  aws.String(cfg.SnapshotID),
		VolumeId:    aws.String(cfg.VolumeID),
		VolumeSize:  aws.Int64(cfg.VolumeSize),
		State:       aws.String(cfg.State),
		Progress:    aws.String(cfg.Progress),
		StartTime:   aws.Time(cfg.StartTime),
		Description: aws.String(cfg.Description),
		Encrypted:   aws.Bool(cfg.Encrypted),
		OwnerId:     aws.String(cfg.OwnerID),
	}

	snapshot.Tags = awstags.MapToEC2(cfg.Tags)

	return snapshot
}

// CreateSnapshot creates a new snapshot from a volume.
func (s *SnapshotServiceImpl) CreateSnapshot(ctx context.Context, input *ec2.CreateSnapshotInput, accountID string) (*ec2.Snapshot, error) {
	if input == nil || input.VolumeId == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	// The caller's account is a key segment, so an untenanted caller has no
	// prefix to read or write under. Refuse at the boundary rather than in the
	// key builder, where the rejection would surface as an internal error.
	if accountID == "" {
		return nil, errors.New(awserrors.ErrorAuthFailure)
	}

	volumeID := *input.VolumeId

	slog.InfoContext(ctx, "CreateSnapshot request", "volumeId", volumeID)

	snapshotID := awsidentifiers.GenerateResourceID("snap")
	if s.provider == nil {
		slog.ErrorContext(ctx, "no EBS provider configured", "op", "CreateSnapshot")
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// The read is already scoped to the caller's prefix, so a returned document
	// is the caller's by construction. The comparison stays as an assertion.
	volume, err := s.metadata.GetVolume(ctx, accountID, volumeID)
	if err != nil || volume.TenantID != accountID {
		return nil, errors.New(awserrors.ErrorInvalidVolumeNotFound)
	}
	if volume.CapacityGiB == 0 {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	// A volume still being prepared has no settled contents to capture.
	if volume.State == string(ebsprovider.VolumeStateCreating) {
		return nil, errors.New(awserrors.ErrorIncorrectState)
	}

	doc := pendingDocument(volume, snapshotID, accountID, aws.StringValue(input.Description))
	doc.Tags = awstags.Extract(input.TagSpecifications, "snapshot")
	return s.createSnapshot(ctx, doc, providerCreateRequest(volume, snapshotID), createHooks{})
}

// pendingDocument is the document a snapshot of volume is recorded as before
// the provider is called. AmbiguousCreate is set because that call is next.
func pendingDocument(volume ebsmetadata.Volume, snapshotID, accountID, description string) ebsmetadata.Snapshot {
	return ebsmetadata.Snapshot{
		SnapshotID: snapshotID, VolumeID: volume.VolumeID,
		VolumeSize: safecast.Uint64ToInt64(volume.CapacityGiB),
		State:      snapshotStatePending, Progress: "0%", StartTime: time.Now(),
		Description: description, Encrypted: volume.Encrypted,
		OwnerID: accountID, AvailabilityZone: volume.AvailabilityZone,
		AmbiguousCreate: true,
	}
}

func providerCreateRequest(volume ebsmetadata.Volume, snapshotID string) ebsprovider.CreateSnapshotRequest {
	return ebsprovider.CreateSnapshotRequest{
		Versioned: ebsprovider.NewVersioned(), SnapshotID: snapshotID, VolumeID: volume.VolumeID, VolumeHandle: volume.ProviderHandle,
	}
}

// ImageSnapshotHooks are the AMI document's writes around an image's snapshot.
// Recorded runs after the snapshot's document and reference and before the
// provider call; its error rolls the snapshot back.
type ImageSnapshotHooks struct {
	Recorded func(ctx context.Context) error
	// Refused removes the AMI document when the provider refused the first call.
	Refused func(ctx context.Context)
	// Completed runs once the snapshot's document is written completed.
	Completed func(ctx context.Context)
}

// ImageSnapshotCreator takes the snapshot behind a CreateImage through the
// same document, reference, claim and provider path as CreateSnapshot.
type ImageSnapshotCreator interface {
	CreateImageSnapshot(ctx context.Context, volume ebsmetadata.Volume, snapshotID, accountID, description string, hooks ImageSnapshotHooks) (*ec2.Snapshot, error)
}

var _ ImageSnapshotCreator = (*SnapshotServiceImpl)(nil)

// ImageClaims serialises the reaper's settling of an AMI with DeregisterImage:
// each holds the image's claim, in the snapshot claim group keyed by image ID.
type ImageClaims interface {
	LockImage(ctx context.Context, imageID string) (unlock func(), err error)
}

var _ ImageClaims = (*SnapshotServiceImpl)(nil)

// LockImage takes imageID's claim for a request, as writeUnderClaim takes a
// snapshot's; one still held after the retries is a retryable IncorrectState.
func (s *SnapshotServiceImpl) LockImage(ctx context.Context, imageID string) (func(), error) {
	held, err := s.takeClaimWithRetry(ctx, "image", imageID)
	if err != nil {
		return nil, err
	}
	return func() { held.release(ctx) }, nil
}

// CreateImageSnapshot snapshots an image's root volume as CreateSnapshot does,
// running hooks to write the AMI document beside the snapshot's.
func (s *SnapshotServiceImpl) CreateImageSnapshot(ctx context.Context, volume ebsmetadata.Volume, snapshotID, accountID, description string, hooks ImageSnapshotHooks) (*ec2.Snapshot, error) {
	if s.provider == nil {
		slog.ErrorContext(ctx, "no EBS provider configured", "op", "CreateImageSnapshot")
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	doc := pendingDocument(volume, snapshotID, accountID, description)
	return s.createSnapshot(ctx, doc, providerCreateRequest(volume, snapshotID), createHooks(hooks))
}

// createHooks are the writes a CreateImage adds around createSnapshot's.
type createHooks ImageSnapshotHooks

// createStep is a point in createSnapshot after which a crash leaves its
// writes so far, for a test to stop the request at.
type createStep int

const (
	stepClaimed createStep = iota + 1
	stepRecorded
	stepReferenced
	stepAnswered
	stepSettled
	stepRollbackMarked
	stepRollbackUnreferenced
)

// errCreateCrashed is what createSnapshot returns when a test stops it at a step.
var errCreateCrashed = errors.New("snapshot create stopped at a test crash point")

// createSnapshot records doc pending and then asks the provider for it, so a
// provider snapshot never exists without a document. Only a refusal of the
// first call removes the document; anything else leaves it for the reaper.
func (s *SnapshotServiceImpl) createSnapshot(ctx context.Context, doc ebsmetadata.Snapshot, req ebsprovider.CreateSnapshotRequest, hooks createHooks) (*ec2.Snapshot, error) {
	snapshotID, volumeID, accountID := doc.SnapshotID, doc.VolumeID, doc.OwnerID

	held, err := s.claims.lock(ctx, snapshotID)
	if err != nil {
		slog.ErrorContext(ctx, "CreateSnapshot could not take the snapshot claim", "snapshotId", snapshotID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	crashed := false
	defer func() {
		if !crashed {
			held.release(ctx)
		}
	}()
	crash := func(step createStep) bool {
		crashed = s.crashAt != nil && s.crashAt(step)
		return crashed
	}
	if crash(stepClaimed) {
		return nil, errCreateCrashed
	}

	// The document comes first because it is what the reaper discovers work
	// from; a reference with no document would block DeleteVolume for good.
	if err := s.metadata.PutSnapshot(ctx, doc); err != nil {
		slog.ErrorContext(ctx, "CreateSnapshot failed to write the pending snapshot document", "snapshotId", snapshotID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if crash(stepRecorded) {
		return nil, errCreateCrashed
	}
	if err := s.addSnapshotRef(ctx, volumeID, snapshotID); err != nil {
		slog.ErrorContext(ctx, "CreateSnapshot failed to add snapshot ref to KV", "snapshotId", snapshotID, "volumeId", volumeID, "err", err)
		if s.rollBackCreate(ctx, doc, crash) {
			return nil, errCreateCrashed
		}
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if crash(stepReferenced) {
		return nil, errCreateCrashed
	}
	if hooks.Recorded != nil {
		if err := hooks.Recorded(ctx); err != nil {
			slog.ErrorContext(ctx, "CreateSnapshot failed to record the image beside its snapshot", "snapshotId", snapshotID, "err", err)
			if s.rollBackCreate(ctx, doc, crash) {
				return nil, errCreateCrashed
			}
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
	}

	// The document carries AmbiguousCreate from its first write, so a crash
	// once this call is sent leaves the flag set.
	created, err := s.provider.CreateSnapshot(ctx, req)
	if crash(stepAnswered) {
		return nil, errCreateCrashed
	}
	if err == nil && created != nil {
		return s.answerCreate(ctx, doc, created, false, hooks, crash)
	}
	if definiteRefusal(err) {
		// The request holds the claim, so no other call for this ID exists that
		// a provider could still accept: the document can go.
		slog.WarnContext(ctx, "CreateSnapshot refused by the provider", "volumeId", volumeID, "snapshotId", snapshotID, "err", err)
		if hooks.Refused != nil {
			hooks.Refused(ctx)
		}
		if s.rollBackCreate(ctx, doc, crash) {
			return nil, errCreateCrashed
		}
		return nil, ProviderCreateError(volumeID, err)
	}

	// The first call may have been accepted, so it is repeated: the provider
	// answers a repeat with the snapshot it already holds.
	slog.WarnContext(ctx, "CreateSnapshot provider call failed ambiguously, repeating it", "volumeId", volumeID, "snapshotId", snapshotID, "err", err)
	created, err = s.provider.CreateSnapshot(ctx, req)
	if crash(stepAnswered) {
		return nil, errCreateCrashed
	}
	if err == nil && created != nil {
		return s.answerCreate(ctx, doc, created, true, hooks, crash)
	}
	if refusalSettlesError(err) {
		slog.WarnContext(ctx, "CreateSnapshot repeat refused for good, marking the snapshot error", "volumeId", volumeID, "snapshotId", snapshotID, "err", err)
		wctx, cancel := writeContext(ctx)
		defer cancel()
		if _, _, werr := s.updatePending(wctx, accountID, snapshotID, markSnapshotError); werr != nil {
			slog.ErrorContext(ctx, "CreateSnapshot failed to mark the snapshot error", "snapshotId", snapshotID, "err", werr)
		}
		return nil, ProviderCreateError(volumeID, err)
	}
	if definiteRefusal(err) {
		// The first call may still be accepted once this refusal clears.
		slog.WarnContext(ctx, "CreateSnapshot repeat refused, leaving the snapshot pending", "volumeId", volumeID, "snapshotId", snapshotID, "err", err)
		return nil, ProviderCreateError(volumeID, err)
	}
	slog.ErrorContext(ctx, "CreateSnapshot provider call failed ambiguously twice, leaving the snapshot pending", "volumeId", volumeID, "snapshotId", snapshotID, "err", err)
	return nil, errors.New(awserrors.ErrorServerInternal)
}

// answerCreate writes the provider's answer onto the pending document and
// returns the snapshot as written. The answer fixed the contents, so a failed
// write still reports success; the reaper settles the document.
func (s *SnapshotServiceImpl) answerCreate(ctx context.Context, doc ebsmetadata.Snapshot, created *ebsprovider.Snapshot,
	earlierUnanswered bool, hooks createHooks, crash func(createStep) bool) (*ec2.Snapshot, error) {
	wctx, cancel := writeContext(ctx)
	defer cancel()
	written, ok, err := s.updatePending(wctx, doc.OwnerID, doc.SnapshotID, applyProviderAnswer(created, earlierUnanswered))
	completed := ok && written.State == snapshotStateCompleted
	switch {
	case err != nil:
		slog.ErrorContext(ctx, "CreateSnapshot failed to record the provider's answer", "snapshotId", doc.SnapshotID, "err", err)
		written = doc
	case !ok && written.SnapshotID == "":
		// Deleted while the provider answered; report what the answer was.
		written = doc
		applyProviderAnswer(created, earlierUnanswered)(&written)
	}
	if crash(stepSettled) {
		return nil, errCreateCrashed
	}
	if completed && hooks.Completed != nil {
		hooks.Completed(wctx)
	}
	slog.InfoContext(ctx, "CreateSnapshot completed", "snapshotId", doc.SnapshotID, "volumeId", doc.VolumeID, "state", written.State)
	return snapshotConfigToEC2(written), nil
}

// rollBackCreate removes a snapshot the provider refused or never saw, in the
// reaper's order: the document is marked deleted with no call outstanding, then
// the reference and the document go. It reports a test crash.
func (s *SnapshotServiceImpl) rollBackCreate(ctx context.Context, doc ebsmetadata.Snapshot, crash func(createStep) bool) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), snapshotCleanupTimeout)
	defer cancel()
	// A marked document a failure below leaves is the reaper's to delete, and it
	// never repeats the create of one, so the refusal the caller saw stands.
	doc.DeleteRequested, doc.AmbiguousCreate = true, false
	if err := s.metadata.PutSnapshot(ctx, doc); err != nil {
		slog.ErrorContext(ctx, "CreateSnapshot failed to mark a refused snapshot's document deleted", "snapshotId", doc.SnapshotID, "err", err)
	}
	if crash(stepRollbackMarked) {
		return true
	}
	if err := s.removeSnapshotRef(ctx, doc.VolumeID, doc.SnapshotID); err != nil {
		slog.ErrorContext(ctx, "CreateSnapshot failed to remove a refused snapshot's ref, leaving its document for the reaper",
			"snapshotId", doc.SnapshotID, "volumeId", doc.VolumeID, "err", err)
		return false
	}
	if crash(stepRollbackUnreferenced) {
		return true
	}
	if err := s.metadata.DeleteSnapshot(ctx, doc.OwnerID, doc.SnapshotID); err != nil && !objectstore.IsNoSuchKeyError(err) {
		slog.ErrorContext(ctx, "CreateSnapshot failed to remove a refused snapshot's document, leaving it for the reaper", "snapshotId", doc.SnapshotID, "err", err)
	}
	return false
}

// describeSnapshotsValidFilters defines the set of filter names accepted by DescribeSnapshots.
var describeSnapshotsValidFilters = map[string]bool{
	"snapshot-id": true,
	"status":      true,
	"volume-id":   true,
	"volume-size": true,
	"owner-id":    true,
	"tag-key":     true,
	"tag-value":   true,
}

// DescribeSnapshots lists snapshots matching the specified criteria, scoped to the caller's account.
func (s *SnapshotServiceImpl) DescribeSnapshots(ctx context.Context, input *ec2.DescribeSnapshotsInput, accountID string) (*ec2.DescribeSnapshotsOutput, error) {
	return s.describeSnapshots(ctx, input, accountID, false)
}

// DescribeSnapshotsStrict is the control-plane lookup contract. A metadata read
// failure makes the result non-authoritative, so it is returned rather than
// hidden as an absent snapshot.
func (s *SnapshotServiceImpl) DescribeSnapshotsStrict(ctx context.Context, input *ec2.DescribeSnapshotsInput, accountID string) (*ec2.DescribeSnapshotsOutput, error) {
	return s.describeSnapshots(ctx, input, accountID, true)
}

func (s *SnapshotServiceImpl) describeSnapshots(ctx context.Context, input *ec2.DescribeSnapshotsInput,
	accountID string, strict bool) (*ec2.DescribeSnapshotsOutput, error) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	slog.InfoContext(ctx, "DescribeSnapshots request", "snapshotIds", input.SnapshotIds, "accountID", accountID)

	parsedFilters, err := awsfilters.ParseFilters(input.Filters, describeSnapshotsValidFilters)
	if err != nil {
		slog.WarnContext(ctx, "DescribeSnapshots: invalid filter", "err", err)
		return nil, err
	}

	if len(input.SnapshotIds) > 0 {
		return s.describeSnapshotsByIDs(ctx, input, accountID, parsedFilters, strict)
	}

	// The caller's prefix is the isolation and the whole answer: no other
	// account's document is read, and OwnerIds either names the caller, whose
	// prefix this is, or an account whose snapshots cannot be under it.
	var configs []ebsmetadata.Snapshot
	if ownerIDsIncludeCaller(input.OwnerIds, accountID) {
		if strict {
			configs, err = s.metadata.ListSnapshotsStrict(ctx, accountID)
		} else {
			configs, err = s.metadata.ListSnapshots(ctx, accountID)
		}
		if err != nil {
			slog.ErrorContext(ctx, "DescribeSnapshots failed to list snapshot documents", "accountID", accountID, "err", err)
			if strict {
				return nil, fmt.Errorf("describe snapshots for %s: %w", accountID, err)
			}
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
	}

	// Every read fails once the caller's deadline passes, and a tolerant listing
	// reports that as an empty list, which reads as "this account has no
	// snapshots".
	if err := ctx.Err(); err != nil {
		slog.WarnContext(ctx, "DescribeSnapshots gave up reading metadata",
			"read", len(configs), "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	var snapshots []*ec2.Snapshot
	for _, cfg := range configs {
		if cfg.Hidden() {
			continue
		}
		if len(parsedFilters) > 0 && !snapshotMatchesFilters(cfg, parsedFilters) {
			continue
		}

		snapshots = append(snapshots, snapshotConfigToEC2(cfg))
	}

	slog.InfoContext(ctx, "DescribeSnapshots completed", "count", len(snapshots))

	return &ec2.DescribeSnapshotsOutput{
		Snapshots: snapshots,
	}, nil
}

// describeSnapshotsByIDs reads each named snapshot's document directly, rather
// than pay a fetch per snapshot the account owns however few it names.
//
// The key is scoped to the caller's account, so a cross-tenant ID reads a key
// that does not exist and reports not-found — the same answer the listing path
// gives, and for the same reason.
//
// strict keeps the control-plane contract the listing path has: the underlying
// metadata failure is returned rather than flattened to an opaque code.
func (s *SnapshotServiceImpl) describeSnapshotsByIDs(ctx context.Context, input *ec2.DescribeSnapshotsInput,
	accountID string, parsedFilters map[string][]string, strict bool) (*ec2.DescribeSnapshotsOutput, error) {
	// Only nil entries names nothing, which the listing path answers with an
	// empty result rather than a missing ID.
	snapshotIDs := awsidentifiers.Distinct(input.SnapshotIds)
	if len(snapshotIDs) == 0 {
		return &ec2.DescribeSnapshotsOutput{}, nil
	}

	// OwnerIds naming only another account selects snapshots the caller's
	// prefix cannot contain, so a named ID is absent without any read.
	if !ownerIDsIncludeCaller(input.OwnerIds, accountID) {
		return nil, errors.New(awserrors.ErrorInvalidSnapshotNotFound)
	}

	// An untenanted caller has no prefix to read under; refuse here rather than
	// report a snapshot that may well exist as absent. The key builder owns the
	// wording, so the strict variant reports what the listing path reports.
	if !awsidentifiers.IsAccountID(accountID) {
		_, keyErr := ebsmetadata.SnapshotKey(accountID, snapshotIDs[0])
		slog.ErrorContext(ctx, "DescribeSnapshots refused a caller with no account", "accountID", accountID)
		if strict {
			return nil, fmt.Errorf("describe snapshots for %s: %w", accountID, keyErr)
		}
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	for _, snapshotID := range snapshotIDs {
		// An ID no key can be built for names no document, which is also what
		// the listing path reports for it.
		if _, err := ebsmetadata.SnapshotKey(accountID, snapshotID); err != nil {
			return nil, errors.New(awserrors.ErrorInvalidSnapshotNotFound)
		}
	}

	results := ebsmetadata.FetchByIDs(ctx, snapshotIDs, func(ctx context.Context, id string) (ebsmetadata.Snapshot, error) {
		return s.metadata.GetSnapshot(ctx, accountID, id)
	})

	// A read that failed once the deadline passed is not evidence the snapshot
	// is absent, which is the only other thing this path reports.
	if err := ctx.Err(); err != nil {
		slog.WarnContext(ctx, "DescribeSnapshots gave up reading snapshot documents",
			"requested", len(snapshotIDs), "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// A named ID is never the unrelated document the tolerant listing's skip
	// exists for, so an unreadable one fails both variants. A failed read also
	// outranks a missing one however the request ordered them.
	var readErr error
	for i, result := range results {
		if result.Err != nil && !objectstore.IsNoSuchKeyError(result.Err) {
			slog.ErrorContext(ctx, "DescribeSnapshots failed to read a named snapshot document",
				"snapshotId", snapshotIDs[i], "accountID", accountID, "err", result.Err)
			readErr = result.Err
		}
	}
	if readErr != nil {
		if strict {
			return nil, fmt.Errorf("describe snapshots for %s: %w", accountID, readErr)
		}
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	snapshots := make([]*ec2.Snapshot, 0, len(snapshotIDs))
	for i, result := range results {
		// Every other store error was classified above, so this is a missing key.
		if result.Err != nil {
			return nil, errors.New(awserrors.ErrorInvalidSnapshotNotFound)
		}

		// The read was keyed by the ID inside the caller's prefix, so the
		// document is theirs and is the one named. Both stay as assertions.
		if result.Document.SnapshotID != snapshotIDs[i] || result.Document.OwnerID != accountID || result.Document.Hidden() {
			return nil, errors.New(awserrors.ErrorInvalidSnapshotNotFound)
		}

		// A named snapshot a filter excludes is not-found, as on the listing path.
		if len(parsedFilters) > 0 && !snapshotMatchesFilters(result.Document, parsedFilters) {
			return nil, errors.New(awserrors.ErrorInvalidSnapshotNotFound)
		}

		snapshots = append(snapshots, snapshotConfigToEC2(result.Document))
	}

	slog.InfoContext(ctx, "DescribeSnapshots completed", "count", len(snapshots))

	return &ec2.DescribeSnapshotsOutput{
		Snapshots: snapshots,
	}, nil
}

// ownerIDsIncludeCaller reports whether an OwnerIds request parameter selects
// the account whose prefix is being listed. "self" and the caller's own account
// ID both name that prefix; any other value names an account the caller's
// prefix cannot contain.
func ownerIDsIncludeCaller(ownerIDs []*string, accountID string) bool {
	if len(ownerIDs) == 0 {
		return true
	}

	for _, owner := range ownerIDs {
		if owner == nil {
			continue
		}
		if *owner == "self" || *owner == accountID {
			return true
		}
	}

	return false
}

// snapshotMatchesFilters checks whether a snapshot document satisfies all parsed awsfilters.
func snapshotMatchesFilters(cfg ebsmetadata.Snapshot, filters map[string][]string) bool {
	for name, values := range filters {
		if awsfilters.IsTagFilter(name) {
			continue
		}

		var field string
		switch name {
		case "snapshot-id":
			field = cfg.SnapshotID
		case "status":
			field = cfg.State
		case "volume-id":
			field = cfg.VolumeID
		case "volume-size":
			field = strconv.FormatInt(cfg.VolumeSize, 10)
		case "owner-id":
			field = cfg.OwnerID
		default:
			return false
		}

		if !awsfilters.MatchesAny(values, field) {
			return false
		}
	}

	return awsfilters.MatchesTags(filters, cfg.Tags)
}

// snapshotInUseByVolumes reports whether any volume was created from the given
// snapshot, and the ID of one owned by accountID if any. A clone records its
// source in its ebsmetadata document; missing one would strip chunks it reads.
func (s *SnapshotServiceImpl) snapshotInUseByVolumes(ctx context.Context, snapshotID, accountID string) (bool, string, error) {
	// Whole-cluster, and not scoping this to the snapshot's own account is the
	// point: launching from a system AMI writes a root volume owned by the
	// launching tenant whose SnapshotID is the system snapshot, so a clone
	// routinely lives outside the snapshot owner's prefix.
	volumes, unreadable, err := s.metadata.ListAllVolumesPartial(ctx)
	if err != nil {
		return false, "", awserrors.Errorf(awserrors.ErrorServerInternal, "check whether snapshot %s is in use: list volumes: %v", snapshotID, err)
	}
	inUse := false
	for _, volume := range volumes {
		// A deleting volume's teardown removes only its own objects and never
		// reads the snapshot again, so it no longer holds the snapshot's chunks.
		if volume.SnapshotID != snapshotID || volume.State == string(ebsprovider.VolumeStateDeleting) {
			continue
		}
		if volume.TenantID == accountID {
			return true, volume.VolumeID, nil
		}
		inUse = true
	}
	if inUse {
		return true, "", nil
	}
	// A volume that failed to read is not evidence that no clone reads the
	// snapshot's chunks, and this answer gates deleting them.
	if len(unreadable) > 0 {
		return false, "", unreadableRecordError(ctx, snapshotID, accountID, "volume", unreadable)
	}
	return false, "", nil
}

// amiInUseBySnapshot checks if any registered AMI names the given snapshot as
// its root device. AWS refuses DeleteSnapshot while a registered AMI
// references the snapshot, the same way it refuses one backing a volume.
func (s *SnapshotServiceImpl) amiInUseBySnapshot(ctx context.Context, snapshotID, accountID string) (bool, error) {
	// Whole-cluster and fail-closed for the same reason as the volume check
	// above: a system AMI can be registered by an account other than the
	// snapshot's owner, and an AMI this cannot read is not evidence none exists.
	amis, unreadable, err := s.metadata.ListAMIsPartial(ctx)
	if err != nil {
		return false, awserrors.Errorf(awserrors.ErrorServerInternal, "check whether snapshot %s is in use: list AMIs: %v", snapshotID, err)
	}
	for _, ami := range amis {
		if ami.SnapshotID == snapshotID {
			return true, nil
		}
	}
	if len(unreadable) > 0 {
		return false, unreadableRecordError(ctx, snapshotID, accountID, "AMI", unreadable)
	}
	return false, nil
}

// unreadableRecordError refuses a delete the in-use scan could not clear. It
// names a record only when the caller owns it: the scan is whole-cluster, and
// an AMI record that will not read cannot say who owns it.
func unreadableRecordError(ctx context.Context, snapshotID, accountID, kind string, unreadable []ebsmetadata.UnreadableDocument) error {
	for _, doc := range unreadable {
		slog.ErrorContext(ctx, "DeleteSnapshot cannot read "+kind+" record", "snapshotId", snapshotID, "key", doc.Key, "err", doc.Err)
	}
	for _, doc := range unreadable {
		if doc.AccountID != "" && doc.AccountID == accountID {
			return awserrors.Errorf(awserrors.ErrorServerInternal,
				"Cannot confirm that snapshot '%s' is not in use: the record for %s '%s' could not be read", snapshotID, kind, doc.ID)
		}
	}
	return awserrors.Errorf(awserrors.ErrorServerInternal,
		"Cannot confirm that snapshot '%s' is not in use: %d %s record(s) could not be read", snapshotID, len(unreadable), kind)
}

// ProviderCreateError maps a provider CreateSnapshot failure of volumeID to
// the code a client can act on: retry-later for a provider that cannot answer
// now, IncorrectState for one that needs the volume detached, else internal.
func ProviderCreateError(volumeID string, err error) error {
	switch {
	case errors.Is(err, ebsprovider.ErrUnavailable):
		return errors.New(awserrors.ErrorServiceUnavailable)
	case errors.Is(err, ebsprovider.ErrVolumeInUse):
		return awserrors.Errorf(awserrors.ErrorIncorrectState,
			"The volume '%s' is in use. Detach it before creating a snapshot.", volumeID)
	default:
		return errors.New(awserrors.ErrorServerInternal)
	}
}

// providerDeleteError maps a provider DeleteSnapshot failure to the code a
// client can act on. A transport failure is retryable: the delete is idempotent.
func providerDeleteError(snapshotID string, err error) error {
	switch {
	case errors.Is(err, ebsprovider.ErrVolumeInUse):
		return awserrors.Errorf(awserrors.ErrorInvalidSnapshotInUse, "The snapshot '%s' is in use by a volume.", snapshotID)
	case errors.Is(err, ebsprovider.ErrUnavailable), errors.Is(err, nats.ErrNoResponders),
		errors.Is(err, nats.ErrConnectionClosed), errors.Is(err, nats.ErrTimeout),
		errors.Is(err, context.DeadlineExceeded):
		return awserrors.Errorf(awserrors.ErrorServiceUnavailable, "delete snapshot %s: storage provider unavailable: %v", snapshotID, err)
	default:
		return awserrors.Errorf(awserrors.ErrorServerInternal, "delete snapshot %s: %v", snapshotID, err)
	}
}

// DeleteSnapshot deletes a snapshot after verifying the caller owns it.
func (s *SnapshotServiceImpl) DeleteSnapshot(ctx context.Context, input *ec2.DeleteSnapshotInput, accountID string) (*ec2.DeleteSnapshotOutput, error) {
	if input == nil || input.SnapshotId == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	// An untenanted caller has no prefix to delete under; refuse here rather
	// than in the key builder.
	if accountID == "" {
		return nil, errors.New(awserrors.ErrorAuthFailure)
	}

	snapshotID := *input.SnapshotId

	slog.InfoContext(ctx, "DeleteSnapshot request", "snapshotId", snapshotID, "accountID", accountID)

	cfg, err := s.getSnapshotConfig(ctx, accountID, snapshotID)
	if err != nil {
		slog.ErrorContext(ctx, "DeleteSnapshot snapshot not found", "snapshotId", snapshotID, "err", err)
		return nil, err
	}

	// The read was scoped to the caller's prefix, so the document is theirs by
	// construction. The comparison stays as an assertion.
	if cfg.OwnerID != accountID {
		slog.WarnContext(ctx, "DeleteSnapshot: account does not own snapshot", "snapshotId", snapshotID, "accountID", accountID, "ownerID", cfg.OwnerID)
		return nil, errors.New(awserrors.ErrorUnauthorizedOperation)
	}

	// Check if any volumes were created from this snapshot
	// A scan's failure is held until both have run, so a readable reference
	// found by either answers InvalidSnapshot.InUse.
	inUse, volumeID, volumeErr := s.snapshotInUseByVolumes(ctx, snapshotID, accountID)
	if inUse {
		slog.InfoContext(ctx, "DeleteSnapshot blocked: snapshot in use by volume", "snapshotId", snapshotID, "volumeId", volumeID)
		// The registered message blames AMIs. Name the volume only when the
		// caller owns it: the scan is whole-cluster.
		if volumeID == "" {
			return nil, awserrors.Errorf(awserrors.ErrorInvalidSnapshotInUse, "The snapshot '%s' is in use by a volume.", snapshotID)
		}
		return nil, awserrors.Errorf(awserrors.ErrorInvalidSnapshotInUse, "The snapshot '%s' is in use by volume '%s'.", snapshotID, volumeID)
	}

	// Check if any registered AMI names this snapshot as its root device
	amiInUse, amiErr := s.amiInUseBySnapshot(ctx, snapshotID, accountID)
	if amiInUse {
		slog.InfoContext(ctx, "DeleteSnapshot blocked: snapshot in use by registered AMI", "snapshotId", snapshotID)
		return nil, errors.New(awserrors.ErrorInvalidSnapshotInUse)
	}
	if volumeErr != nil {
		slog.ErrorContext(ctx, "DeleteSnapshot failed to check snapshot usage", "snapshotId", snapshotID, "err", volumeErr)
		return nil, volumeErr
	}
	if amiErr != nil {
		slog.ErrorContext(ctx, "DeleteSnapshot failed to check AMI usage", "snapshotId", snapshotID, "err", amiErr)
		return nil, amiErr
	}
	if s.provider == nil {
		slog.ErrorContext(ctx, "no EBS provider configured", "op", "DeleteSnapshot")
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// A provider may refuse to delete a snapshot still in progress, and an
	// unanswered create may still be accepted, so either is marked for the reaper.
	if deferDelete(cfg) {
		marked, current, err := s.requestDelete(ctx, cfg)
		if err != nil {
			return nil, err
		}
		if marked {
			slog.InfoContext(ctx, "DeleteSnapshot accepted; the snapshot is deleted once it settles", "snapshotId", snapshotID)
			return &ec2.DeleteSnapshotOutput{}, nil
		}
		cfg = current
	}
	if err := s.provider.DeleteSnapshot(ctx, ebsprovider.DeleteSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: snapshotID, Handle: cfg.ProviderHandle}); err != nil {
		slog.ErrorContext(ctx, "DeleteSnapshot provider deletion failed", "snapshotId", snapshotID, "err", err)
		return nil, providerDeleteError(snapshotID, err)
	}

	// The provider has removed the snapshot's storage. The document goes after
	// it, so a provider failure above leaves a snapshot DescribeSnapshots shows
	// and DeleteSnapshot can retry against an idempotent provider delete.
	if err := s.metadata.DeleteSnapshot(ctx, accountID, snapshotID); err != nil && !objectstore.IsNoSuchKeyError(err) {
		slog.ErrorContext(ctx, "DeleteSnapshot failed to delete metadata document", "snapshotId", snapshotID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Remove from KV after the document. Failure is logged but not fatal —
	// a phantom entry safely blocks volume deletion rather than allowing it.
	if err := s.removeSnapshotRefForCleanup(ctx, cfg.VolumeID, snapshotID); err != nil {
		slog.Warn("DeleteSnapshot failed to remove snapshot ref from KV", "snapshotId", snapshotID, "volumeId", cfg.VolumeID, "err", err)
	}

	slog.InfoContext(ctx, "DeleteSnapshot completed", "snapshotId", snapshotID)

	return &ec2.DeleteSnapshotOutput{}, nil
}

// deferDelete reports a snapshot DeleteSnapshot marks rather than deletes.
func deferDelete(doc ebsmetadata.Snapshot) bool {
	return doc.State == snapshotStatePending || doc.AmbiguousCreate
}

// requestDelete marks doc DeleteRequested under its claim. A document that
// settled with no call outstanding since it was read is returned unmarked,
// for the caller to delete as any other.
func (s *SnapshotServiceImpl) requestDelete(ctx context.Context, doc ebsmetadata.Snapshot) (bool, ebsmetadata.Snapshot, error) {
	marked := false
	var current ebsmetadata.Snapshot
	err := s.writeUnderClaim(ctx, doc.OwnerID, doc.SnapshotID, func(d *ebsmetadata.Snapshot) (bool, error) {
		if d.Hidden() {
			return false, errors.New(awserrors.ErrorInvalidSnapshotNotFound)
		}
		current = *d
		if !deferDelete(*d) {
			return false, nil
		}
		d.DeleteRequested = true
		marked = true
		return true, nil
	})
	if err != nil {
		return false, ebsmetadata.Snapshot{}, err
	}
	if current.SnapshotID == "" {
		return false, ebsmetadata.Snapshot{}, errors.New(awserrors.ErrorInvalidSnapshotNotFound)
	}
	return marked, current, nil
}

// Contention on a snapshot's claim is bounded: three retries over two seconds.
const (
	claimRetries    = 3
	claimRetryDelay = 2 * time.Second / claimRetries
)

// writeUnderClaim rereads snapshotID's document under its claim and writes it
// when mutate asks to, so no write lands between another writer's reread and
// its write. A document that is gone is not mutated, and mutate never sees it.
func (s *SnapshotServiceImpl) writeUnderClaim(ctx context.Context, accountID, snapshotID string,
	mutate func(*ebsmetadata.Snapshot) (bool, error)) error {
	held, err := s.takeClaimWithRetry(ctx, "snapshot", snapshotID)
	if err != nil {
		return err
	}
	defer held.release(ctx)
	doc, err := s.metadata.GetSnapshot(ctx, accountID, snapshotID)
	if err != nil {
		if objectstore.IsNoSuchKeyError(err) {
			return nil
		}
		return awserrors.Errorf(awserrors.ErrorServerInternal, "read snapshot %s: %v", snapshotID, err)
	}
	write, err := mutate(&doc)
	if err != nil || !write {
		return err
	}
	if err := s.metadata.PutSnapshot(ctx, doc); err != nil {
		return awserrors.Errorf(awserrors.ErrorServerInternal, "write snapshot %s: %v", snapshotID, err)
	}
	return nil
}

// takeClaimWithRetry takes the claim on resource id, of kind, as the
// reaper does, retrying a holder that is live; one still held after the
// retries is IncorrectState, which the caller retries.
func (s *SnapshotServiceImpl) takeClaimWithRetry(ctx context.Context, kind, id string) (*heldClaim, error) {
	delay := claimRetryDelay
	if s.claimRetryDelay > 0 {
		delay = s.claimRetryDelay
	}
	for attempt := 0; ; attempt++ {
		held, err := s.claims.take(ctx, id)
		if err == nil {
			return held, nil
		}
		if !errors.Is(err, errClaimHeld) {
			slog.ErrorContext(ctx, "Snapshot claim could not be taken", "kind", kind, "id", id, "err", err)
			return nil, awserrors.Errorf(awserrors.ErrorServerInternal, "take the claim of %s %s: %v", kind, id, err)
		}
		if s.claimRefused != nil {
			s.claimRefused(id)
		}
		if attempt == claimRetries {
			slog.InfoContext(ctx, "Snapshot claim still held after retries", "kind", kind, "id", id, "err", err)
			return nil, awserrors.Errorf(awserrors.ErrorIncorrectState,
				"The %s '%s' is being updated. Retry the request.", kind, id)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, awserrors.Errorf(awserrors.ErrorServerInternal, "take the claim of %s %s: %v", kind, id, ctx.Err())
		case <-timer.C:
		}
	}
}

// CopySnapshot duplicates a snapshot under a new ID owned by the caller.
// Served only through the provider: the copy needs its own snapshot metadata
// on the backend, and on an encrypted volume that metadata is AEAD-sealed with
// the snapshot ID in the AAD, so only the storage engine can re-seal it.
func (s *SnapshotServiceImpl) CopySnapshot(ctx context.Context, input *ec2.CopySnapshotInput, accountID string) (*ec2.CopySnapshotOutput, error) {
	if input == nil || input.SourceSnapshotId == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	// The copy is written under the caller's account, so an untenanted caller
	// has nowhere to put it; refuse here rather than in the key builder.
	if accountID == "" {
		return nil, errors.New(awserrors.ErrorAuthFailure)
	}

	sourceSnapshotID := *input.SourceSnapshotId

	slog.InfoContext(ctx, "CopySnapshot request", "sourceSnapshotId", sourceSnapshotID, "accountID", accountID)

	sourceCfg, err := s.getSnapshotConfig(ctx, accountID, sourceSnapshotID)
	if err != nil {
		slog.ErrorContext(ctx, "CopySnapshot source snapshot not found", "snapshotId", sourceSnapshotID, "err", err)
		return nil, err
	}

	// Assertion: the read was already scoped to the caller's prefix.
	if sourceCfg.OwnerID != accountID {
		slog.WarnContext(ctx, "CopySnapshot: account does not own source snapshot", "snapshotId", sourceSnapshotID, "accountID", accountID, "ownerID", sourceCfg.OwnerID)
		return nil, errors.New(awserrors.ErrorUnauthorizedOperation)
	}
	if !sourceCfg.Ready() {
		return nil, awserrors.Errorf(awserrors.ErrorIncorrectState,
			"Snapshot is in invalid state - %s", sourceCfg.State)
	}

	// Without a provider the only reachable path writes the control-plane
	// config alone. That copy described and created volumes fine, then failed
	// at attach with no backend metadata behind the new ID, so refuse instead.
	if s.provider == nil {
		slog.WarnContext(ctx, "CopySnapshot is not supported without an EBS provider", "sourceSnapshotId", sourceSnapshotID)
		return nil, errors.New(awserrors.ErrorUnsupportedOperation)
	}

	newSnapshotID := awsidentifiers.GenerateResourceID("snap")
	copied, err := s.provider.CopySnapshot(ctx, ebsprovider.CopySnapshotRequest{
		Versioned:             ebsprovider.NewVersioned(),
		SourceSnapshotID:      sourceSnapshotID,
		DestinationSnapshotID: newSnapshotID,
		VolumeID:              sourceCfg.VolumeID,
	})
	if err != nil || copied == nil {
		slog.ErrorContext(ctx, "CopySnapshot: provider copy failed", "sourceSnapshotId", sourceSnapshotID, "newSnapshotId", newSnapshotID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	now := time.Now()
	snapshotCfg := ebsmetadata.Snapshot{
		SnapshotID: newSnapshotID, VolumeID: sourceCfg.VolumeID,
		VolumeSize: sourceCfg.VolumeSize,
		State:      string(copied.State), Progress: "100%", StartTime: now,
		Encrypted: sourceCfg.Encrypted,
		OwnerID:   accountID, AvailabilityZone: sourceCfg.AvailabilityZone,
		Tags:           awstags.Extract(input.TagSpecifications, "snapshot"),
		ProviderHandle: copied.Handle,
	}
	if snapshotCfg.State == "" {
		snapshotCfg.State = "completed"
	}
	snapshotCfg.Description = sourceCfg.Description
	if input.Description != nil {
		snapshotCfg.Description = *input.Description
	}

	// The copy pins the same chunks as its source, so it must join the volume's
	// snapshot list before it is visible: a copy missing from that list would
	// let the volume be deleted out from under it.
	if err := s.addSnapshotRef(ctx, sourceCfg.VolumeID, newSnapshotID); err != nil {
		_ = s.provider.DeleteSnapshot(ctx, ebsprovider.DeleteSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: newSnapshotID, Handle: copied.Handle})
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if err := s.metadata.PutSnapshot(ctx, snapshotCfg); err != nil {
		_ = s.removeSnapshotRefForCleanup(ctx, sourceCfg.VolumeID, newSnapshotID)
		_ = s.provider.DeleteSnapshot(ctx, ebsprovider.DeleteSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: newSnapshotID, Handle: copied.Handle})
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.InfoContext(ctx, "CopySnapshot completed", "sourceSnapshotId", sourceSnapshotID, "newSnapshotId", newSnapshotID)

	return &ec2.CopySnapshotOutput{
		SnapshotId: aws.String(newSnapshotID),
		Tags:       awstags.MapToEC2(snapshotCfg.Tags),
	}, nil
}

// addSnapshotRef adds snapshotID to the volume's snapshot list in KV.
// Uses CAS (Create/Update with revision) to prevent lost updates under concurrency.
func (s *SnapshotServiceImpl) addSnapshotRef(ctx context.Context, volumeID, snapshotID string) error {
	if s.snapRefs == nil {
		slog.Debug("addSnapshotRef: snapshotKV is nil, skipping", "volumeId", volumeID, "snapshotId", snapshotID)
		return nil
	}

	err := s.snapRefs.Upsert(ctx, volumeID, func(snapshots *[]string) (bool, error) {
		*snapshots = append(*snapshots, snapshotID)
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("addSnapshotRef: failed to record %s against volume %s: %w", snapshotID, volumeID, err)
	}

	slog.Info("addSnapshotRef: added snapshot ref", "volumeId", volumeID, "snapshotId", snapshotID)
	return nil
}

// ensureSnapshotRef adds snapshotID to the volume's snapshot list unless it
// is already there, restoring a reference a crashed request did not write.
func (s *SnapshotServiceImpl) ensureSnapshotRef(ctx context.Context, volumeID, snapshotID string) error {
	if s.snapRefs == nil {
		return nil
	}
	err := s.snapRefs.Upsert(ctx, volumeID, func(snapshots *[]string) (bool, error) {
		if slices.Contains(*snapshots, snapshotID) {
			return false, nil
		}
		*snapshots = append(*snapshots, snapshotID)
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("ensureSnapshotRef: failed to record %s against volume %s: %w", snapshotID, volumeID, err)
	}
	return nil
}

// removeSnapshotRefForCleanup removes a reference with a bounded context that
// survives request cancellation after the snapshot operation has committed.
func (s *SnapshotServiceImpl) removeSnapshotRefForCleanup(ctx context.Context, volumeID, snapshotID string) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), snapshotCleanupTimeout)
	defer cancel()
	return s.removeSnapshotRef(cleanupCtx, volumeID, snapshotID)
}

// removeSnapshotRef removes snapshotID from the volume's snapshot list in KV.
// Deletes the key if the list becomes empty.
// Uses CAS (Update with revision) to prevent lost updates under concurrency.
func (s *SnapshotServiceImpl) removeSnapshotRef(ctx context.Context, volumeID, snapshotID string) error {
	if s.snapRefs == nil {
		slog.Debug("removeSnapshotRef: snapshotKV is nil, skipping", "volumeId", volumeID, "snapshotId", snapshotID)
		return nil
	}

	// A removal that finds nothing commits nothing: an identical record would
	// still bump the revision, waking every watcher and costing the concurrent
	// writers a CAS attempt each.
	var removed, nowEmpty bool
	err := s.snapRefs.Mutate(ctx, volumeID, func(snapshots *[]string) (bool, error) {
		before := len(*snapshots)
		filtered := (*snapshots)[:0]
		for _, snap := range *snapshots {
			if snap != snapshotID {
				filtered = append(filtered, snap)
			}
		}
		*snapshots = filtered
		removed = len(filtered) != before
		nowEmpty = len(filtered) == 0
		return removed, nil
	})
	if errors.Is(err, kvstore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("removeSnapshotRef: failed to drop %s from volume %s: %w", snapshotID, volumeID, err)
	}

	// Still runs when nothing was removed, so a key left empty by a lost drop
	// race is not stranded until the volume gains another snapshot.
	if nowEmpty {
		if err := s.dropEmptySnapshotRefs(ctx, volumeID); err != nil {
			return err
		}
	}

	if removed {
		slog.Info("removeSnapshotRef: removed snapshot ref", "volumeId", volumeID, "snapshotId", snapshotID)
	}
	return nil
}

// dropEmptySnapshotRefs deletes a volume's index key once nothing references it,
// so a volume that has lost its last snapshot leaves no key behind.
//
// The delete is guarded on the revision the emptiness was read at, so a
// CreateSnapshot landing in between keeps its key rather than having it deleted
// out from under it.
func (s *SnapshotServiceImpl) dropEmptySnapshotRefs(ctx context.Context, volumeID string) error {
	snapshots, rev, err := s.snapRefs.Get(ctx, volumeID)
	if err != nil {
		if errors.Is(err, kvstore.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("dropEmptySnapshotRefs: failed to get KV key %s: %w", volumeID, err)
	}
	if len(*snapshots) > 0 {
		return nil
	}
	// A lost race means a snapshot was added back, so the key has to stay.
	if err := s.snapRefs.CompareAndDelete(ctx, volumeID, rev); err != nil &&
		!errors.Is(err, kvstore.ErrConflict) {
		return fmt.Errorf("dropEmptySnapshotRefs: failed to delete KV key %s: %w", volumeID, err)
	}
	return nil
}

// volumeHasSnapshots returns true if the volume has any snapshots in KV.
func (s *SnapshotServiceImpl) volumeHasSnapshots(ctx context.Context, volumeID string) (bool, error) {
	if s.snapRefs == nil {
		return false, nil
	}

	snapshots, _, err := s.snapRefs.Get(ctx, volumeID)
	if err != nil {
		if errors.Is(err, kvstore.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("volumeHasSnapshots: failed to get KV key %s: %w", volumeID, err)
	}

	return len(*snapshots) > 0, nil
}

// ApplyRecordTags mirrors CreateTags into the owning snapshot metadata so
// DescribeSnapshots observes tags added after create. Non-snap ids, snapshots
// absent from this store, and snapshots the caller does not own are skipped.
func (s *SnapshotServiceImpl) ApplyRecordTags(input *ec2.CreateTagsInput, accountID string) error {
	if input == nil {
		return nil
	}
	return s.mirrorSnapshotTags(input.Resources, accountID, tagmirror.MergeTagsMut(input))
}

// RemoveRecordTags mirrors DeleteTags into the owning snapshot metadata with
// AWS-faithful delete semantics.
func (s *SnapshotServiceImpl) RemoveRecordTags(input *ec2.DeleteTagsInput, accountID string) error {
	if input == nil {
		return nil
	}
	return s.mirrorSnapshotTags(input.Resources, accountID, tagmirror.RemoveTagsMut(input))
}

// mirrorSnapshotTags read-modify-writes the snapshot document's Tags for each
// snap- id. The document is read under the caller's own prefix, so a snapshot
// they do not own is simply absent and no-ops.
func (s *SnapshotServiceImpl) mirrorSnapshotTags(resources []*string, accountID string, mut func(map[string]string)) error {
	for _, res := range resources {
		if res == nil || !strings.HasPrefix(*res, "snap-") {
			continue
		}
		cfg, err := s.metadata.GetSnapshot(context.Background(), accountID, *res)
		if err != nil {
			if objectstore.IsNoSuchKeyError(err) {
				continue
			}
			return err
		}
		if cfg.Hidden() {
			continue
		}
		apply := func(d *ebsmetadata.Snapshot) {
			if d.Tags == nil {
				d.Tags = map[string]string{}
			}
			mut(d.Tags)
		}
		// A document the reaper or a DeleteSnapshot may still write is written
		// under its claim, so the tags cannot undo their write or it theirs.
		if deferDelete(cfg) {
			err := s.writeUnderClaim(context.Background(), accountID, *res, func(d *ebsmetadata.Snapshot) (bool, error) {
				if d.Hidden() {
					return false, nil
				}
				apply(d)
				return true, nil
			})
			if err != nil {
				return err
			}
			continue
		}
		apply(&cfg)
		if err := s.metadata.PutSnapshot(context.Background(), cfg); err != nil {
			return err
		}
	}
	return nil
}
