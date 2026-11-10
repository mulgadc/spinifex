package snapshot

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvsync"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	pendingVolumeID = "vol-pending0000001"
	nodeA           = "node-a"
	nodeB           = "node-b"
	nodeC           = "node-c"
)

// errAmbiguous is a CreateSnapshot reply that never arrived.
var errAmbiguous = context.DeadlineExceeded

// scriptedProvider is the memory provider with hooks on CreateSnapshot and
// ListSnapshots, and a record of every delete it is asked for.
type scriptedProvider struct {
	*ebsprovider.MemoryProvider

	mu      sync.Mutex
	creates int
	// onCreate answers call n (from 1) instead of the memory provider when it
	// returns handled; held calls are the ones a test accepts late.
	onCreate func(n int, req ebsprovider.CreateSnapshotRequest) (handled bool, snap *ebsprovider.Snapshot, err error)
	onList   func(req ebsprovider.ListSnapshotsRequest) (handled bool, resp *ebsprovider.ListSnapshotsResponse, err error)
	// onDelete answers a DeleteSnapshot instead of the memory provider when set.
	onDelete func(req ebsprovider.DeleteSnapshotRequest) error
	deletes  []string
}

var _ ebsprovider.EBSProvider = (*scriptedProvider)(nil)

func (p *scriptedProvider) CreateSnapshot(ctx context.Context, req ebsprovider.CreateSnapshotRequest) (*ebsprovider.Snapshot, error) {
	p.mu.Lock()
	p.creates++
	n, hook := p.creates, p.onCreate
	p.mu.Unlock()
	if hook != nil {
		if handled, snap, err := hook(n, req); handled {
			return snap, err
		}
	}
	return p.MemoryProvider.CreateSnapshot(ctx, req)
}

func (p *scriptedProvider) ListSnapshots(ctx context.Context, req ebsprovider.ListSnapshotsRequest) (*ebsprovider.ListSnapshotsResponse, error) {
	p.mu.Lock()
	hook := p.onList
	p.mu.Unlock()
	if hook != nil {
		if handled, resp, err := hook(req); handled {
			return resp, err
		}
	}
	return p.MemoryProvider.ListSnapshots(ctx, req)
}

func (p *scriptedProvider) DeleteSnapshot(ctx context.Context, req ebsprovider.DeleteSnapshotRequest) error {
	p.mu.Lock()
	p.deletes = append(p.deletes, "snapshot "+req.SnapshotID)
	hook := p.onDelete
	p.mu.Unlock()
	if hook != nil {
		return hook(req)
	}
	return p.MemoryProvider.DeleteSnapshot(ctx, req)
}

func (p *scriptedProvider) deleteCalls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.deletes...)
}

func (p *scriptedProvider) DeleteVolume(ctx context.Context, req ebsprovider.DeleteVolumeRequest) error {
	p.mu.Lock()
	p.deletes = append(p.deletes, "volume "+req.VolumeID)
	p.mu.Unlock()
	return p.MemoryProvider.DeleteVolume(ctx, req)
}

func (p *scriptedProvider) createCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.creates
}

// refuse answers every CreateSnapshot from call from on with err, accepting nothing.
func refuseFrom(from int, err error) func(int, ebsprovider.CreateSnapshotRequest) (bool, *ebsprovider.Snapshot, error) {
	return func(n int, _ ebsprovider.CreateSnapshotRequest) (bool, *ebsprovider.Snapshot, error) {
		if n < from {
			return false, nil, nil
		}
		return true, nil, err
	}
}

// fakeLiveness answers LastSeen from a map; a node absent from it has no heartbeat.
type fakeLiveness struct {
	mu   sync.Mutex
	seen map[string]time.Time
	err  map[string]error
}

func (l *fakeLiveness) LastSeen(_ context.Context, node string) (time.Time, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.err[node]; err != nil {
		return time.Time{}, false, err
	}
	seen, ok := l.seen[node]
	return seen, ok, nil
}

func (l *fakeLiveness) beat(node string, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen[node] = at
}

// faultStore is an object store whose snapshot document writes and deletes
// can be failed, and which records every delete it is asked for.
type faultStore struct {
	objectstore.ObjectStore

	mu         sync.Mutex
	putsBefore    int // snapshot document puts allowed before failPuts applies; -1 never fails
	failDelete    bool
	failVolumeGet bool
	deletes       []string
}

const volumeDocumentPrefix = "spinifex/ebsmetadata/v2/volumes/"

func (s *faultStore) GetObject(ctx context.Context, input *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	s.mu.Lock()
	fail := s.failVolumeGet && strings.HasPrefix(aws.StringValue(input.Key), volumeDocumentPrefix)
	s.mu.Unlock()
	if fail {
		return nil, errors.New("simulated volume document read failure")
	}
	return s.ObjectStore.GetObject(ctx, input)
}

func (s *faultStore) PutObject(ctx context.Context, input *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
	if strings.HasPrefix(aws.StringValue(input.Key), snapshotDocumentPrefix) {
		s.mu.Lock()
		fail := s.putsBefore == 0
		if s.putsBefore > 0 {
			s.putsBefore--
		}
		s.mu.Unlock()
		if fail {
			return nil, errors.New("simulated snapshot document write failure")
		}
	}
	return s.ObjectStore.PutObject(ctx, input)
}

func (s *faultStore) DeleteObject(ctx context.Context, input *s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error) {
	s.mu.Lock()
	s.deletes = append(s.deletes, aws.StringValue(input.Key))
	fail := s.failDelete
	s.mu.Unlock()
	if fail {
		return nil, errors.New("simulated delete failure")
	}
	return s.ObjectStore.DeleteObject(ctx, input)
}

// pendingFixture is one cluster: a shared object store, provider, reference
// index and sync bucket, and a service per node built over them.
type pendingFixture struct {
	t        *testing.T
	natsURL  string
	store    *faultStore
	mem      *ebsprovider.MemoryProvider
	provider *scriptedProvider
	js       jetstream.JetStream
	refs     jetstream.KeyValue
	liveness *fakeLiveness
	metadata *ebsmetadata.Store
}

func newPendingFixture(t *testing.T) *pendingFixture {
	t.Helper()
	ns, _, js := testutil.StartTestJetStream(t)
	refs, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: KVBucketVolumeSnapshots})
	require.NoError(t, err)
	mem := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{})
	f := &pendingFixture{
		t:        t,
		natsURL:  ns.ClientURL(),
		store:    &faultStore{ObjectStore: objectstore.NewMemoryObjectStore(), putsBefore: -1},
		mem:      mem,
		provider: &scriptedProvider{MemoryProvider: mem},
		js:       js,
		refs:     refs,
		liveness: &fakeLiveness{seen: map[string]time.Time{}, err: map[string]error{}},
	}
	f.metadata = ebsmetadata.NewStore(f.store, "test-bucket")
	for _, node := range []string{nodeA, nodeB, nodeC} {
		f.liveness.beat(node, time.Now())
	}
	require.NoError(t, f.metadata.PutVolume(t.Context(), ebsmetadata.Volume{
		VolumeID: pendingVolumeID, TenantID: testAccountID, CapacityGiB: 4, State: "available",
		AvailabilityZone: "us-east-1a", ProviderHandle: "memory://volume/" + pendingVolumeID,
	}))
	_, err = mem.CreateVolume(t.Context(), ebsprovider.CreateVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: pendingVolumeID,
		CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 4 << 30},
	})
	require.NoError(t, err)
	return f
}

// node builds a process of node: a service with its own claims over a fresh
// handle on the shared sync bucket, as a restarted daemon has.
func (f *pendingFixture) node(name string) *SnapshotServiceImpl {
	return f.nodeOver(name, kvstore.OpenSyncBucket(f.js))
}

func (f *pendingFixture) nodeOver(name string, sync kvsync.KVStore) *SnapshotServiceImpl {
	cfg := &config.Config{Predastore: config.PredastoreConfig{Bucket: "test-bucket"}}
	svc := NewSnapshotServiceImplWithStore(cfg, f.store, nil, f.refs)
	svc.SetEBSProvider(f.provider)
	svc.SetClaims(NewClaims(NewClaimGroup(sync, name), name, f.liveness))
	return svc
}

func (f *pendingFixture) create(svc *SnapshotServiceImpl) (*ec2.Snapshot, error) {
	return svc.CreateSnapshot(f.t.Context(), &ec2.CreateSnapshotInput{VolumeId: aws.String(pendingVolumeID)}, testAccountID)
}

func (f *pendingFixture) sweep(svc *SnapshotServiceImpl) {
	f.t.Helper()
	_, err := svc.NewPendingSnapshotReaper().Sweep(f.t.Context())
	require.NoError(f.t, err)
}

// docs returns every snapshot document in the cluster.
func (f *pendingFixture) docs() []ebsmetadata.Snapshot {
	f.t.Helper()
	docs, err := f.metadata.ListAllSnapshotsStrict(f.t.Context())
	require.NoError(f.t, err)
	return docs
}

// onlyDoc is the cluster's one snapshot document.
func (f *pendingFixture) onlyDoc() ebsmetadata.Snapshot {
	f.t.Helper()
	docs := f.docs()
	require.Len(f.t, docs, 1)
	return docs[0]
}

func (f *pendingFixture) doc(snapshotID string) (ebsmetadata.Snapshot, bool) {
	f.t.Helper()
	doc, err := f.metadata.GetSnapshot(f.t.Context(), testAccountID, snapshotID)
	if objectstore.IsNoSuchKeyError(err) {
		return ebsmetadata.Snapshot{}, false
	}
	require.NoError(f.t, err)
	return doc, true
}

func (f *pendingFixture) seedPending(snapshotID string) {
	f.t.Helper()
	require.NoError(f.t, f.metadata.PutSnapshot(f.t.Context(), ebsmetadata.Snapshot{
		SnapshotID: snapshotID, VolumeID: pendingVolumeID, VolumeSize: 4, State: snapshotStatePending,
		Progress: "0%", StartTime: time.Now(), OwnerID: testAccountID, AvailabilityZone: "us-east-1a",
	}))
}

// claimHolder reports who holds snapshotID's claim, empty when nobody does.
func (f *pendingFixture) claimHolder(snapshotID string) string {
	f.t.Helper()
	tok, ok, err := kvsync.NewClaim(kvstore.OpenSyncBucket(f.js), ClaimPrefix+snapshotID, "observer").Holder(f.t.Context())
	require.NoError(f.t, err)
	if !ok {
		return ""
	}
	return tok.Holder
}

// holdClaim takes snapshotID's claim as node, as a request there would.
func (f *pendingFixture) holdClaim(node, snapshotID string) {
	f.t.Helper()
	_, err := NewClaimGroup(kvstore.OpenSyncBucket(f.js), node).Get(snapshotID).TryLock(f.t.Context())
	require.NoError(f.t, err)
}

func (f *pendingFixture) refsOf(volumeID string) []string {
	f.t.Helper()
	entry, err := f.refs.Get(f.t.Context(), volumeID)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil
	}
	require.NoError(f.t, err)
	refs, _, err := kvstore.Over[[]string](nil, f.refs, snapshotRefsConfig()).Get(f.t.Context(), volumeID)
	require.NoError(f.t, err, "entry %v", entry)
	return *refs
}

func (f *pendingFixture) providerHolds(snapshotID string) (*ebsprovider.Snapshot, bool) {
	return f.mem.Snapshot(snapshotID)
}

// unreachableRefs is the reference index on a connection that is already closed.
func (f *pendingFixture) unreachableRefs() *kvstore.Store[[]string] {
	f.t.Helper()
	nc, err := nats.Connect(f.natsURL)
	require.NoError(f.t, err)
	js, err := jetstream.New(nc)
	require.NoError(f.t, err)
	kv, err := js.KeyValue(f.t.Context(), KVBucketVolumeSnapshots)
	require.NoError(f.t, err)
	nc.Close()
	return kvstore.Over[[]string](nil, kv, snapshotRefsConfig())
}

// unreachableSync is a sync bucket on a connection that is already closed.
func (f *pendingFixture) unreachableSync() kvsync.KVStore {
	f.t.Helper()
	nc, err := nats.Connect(f.natsURL)
	require.NoError(f.t, err)
	js, err := jetstream.New(nc)
	require.NoError(f.t, err)
	nc.Close()
	return kvstore.OpenSyncBucket(js)
}

func TestCreateSnapshot_WritesThePendingDocumentBeforeCallingTheProvider(t *testing.T) {
	f := newPendingFixture(t)
	svc := f.node(nodeA)
	var seen ebsmetadata.Snapshot
	var seenRefs []string
	var holder string
	f.provider.onCreate = func(_ int, req ebsprovider.CreateSnapshotRequest) (bool, *ebsprovider.Snapshot, error) {
		seen, _ = f.doc(req.SnapshotID)
		seenRefs = f.refsOf(pendingVolumeID)
		holder = f.claimHolder(req.SnapshotID)
		return false, nil, nil
	}

	out, err := f.create(svc)
	require.NoError(t, err)

	id := aws.StringValue(out.SnapshotId)
	assert.Equal(t, snapshotStatePending, seen.State, "the document must exist, pending, before the provider is called")
	assert.Equal(t, "0%", seen.Progress)
	assert.Equal(t, []string{id}, seenRefs, "the volume's reference must be written before the provider is called")
	assert.Equal(t, nodeA, holder, "the request must hold the claim while it calls the provider")
	assert.Empty(t, f.claimHolder(id), "the claim must be released after the last write")
}

// Each provider answer to the request, and what the document is left as.
func TestCreateSnapshot_ProviderAnswers(t *testing.T) {
	cases := []struct {
		name      string
		setup     func(f *pendingFixture)
		wantErr   string // empty: success
		wantState string // empty: no document and no reference
		wantCalls int
		wantHeld  bool // the provider holds the snapshot afterwards
	}{
		{name: "completed", wantState: snapshotStateCompleted, wantCalls: 1, wantHeld: true},
		{name: "pending", setup: func(f *pendingFixture) { f.mem.AnswerSnapshotsPending(true) },
			wantState: snapshotStatePending, wantCalls: 1, wantHeld: true},
		{name: "first call volume_in_use", setup: func(f *pendingFixture) { f.provider.onCreate = refuseFrom(1, ebsprovider.ErrVolumeInUse) },
			wantErr: awserrors.ErrorIncorrectState, wantCalls: 1},
		{name: "first call not_found", setup: func(f *pendingFixture) { f.provider.onCreate = refuseFrom(1, ebsprovider.ErrNotFound) },
			wantErr: awserrors.ErrorServerInternal, wantCalls: 1},
		{name: "first call invalid_argument", setup: func(f *pendingFixture) { f.provider.onCreate = refuseFrom(1, ebsprovider.ErrInvalidArgument) },
			wantErr: awserrors.ErrorServerInternal, wantCalls: 1},
		{name: "first call already_exists", setup: func(f *pendingFixture) { f.provider.onCreate = refuseFrom(1, ebsprovider.ErrAlreadyExists) },
			wantErr: awserrors.ErrorServerInternal, wantCalls: 1},
		{name: "ambiguous, then the repeat returns the accepted snapshot",
			setup:     func(f *pendingFixture) { f.mem.FailCreateSnapshotsAfterAccept(1, nil) },
			wantState: snapshotStateCompleted, wantCalls: 2, wantHeld: true},
		{name: "unavailable after accepting, then the repeat answers pending",
			setup: func(f *pendingFixture) {
				f.mem.AnswerSnapshotsPending(true)
				f.mem.FailCreateSnapshotsAfterAccept(1, ebsprovider.ErrUnavailable)
			},
			wantState: snapshotStatePending, wantCalls: 2, wantHeld: true},
		{name: "ambiguous, then the repeat is volume_in_use",
			setup:   func(f *pendingFixture) { f.provider.onCreate = scriptCalls(errAmbiguous, ebsprovider.ErrVolumeInUse) },
			wantErr: awserrors.ErrorIncorrectState, wantState: snapshotStatePending, wantCalls: 2},
		{name: "ambiguous, then the repeat is invalid_argument",
			setup: func(f *pendingFixture) {
				f.provider.onCreate = scriptCalls(errAmbiguous, ebsprovider.ErrInvalidArgument)
			},
			wantErr: awserrors.ErrorServerInternal, wantState: snapshotStatePending, wantCalls: 2},
		{name: "ambiguous, then the repeat is not_found",
			setup:   func(f *pendingFixture) { f.provider.onCreate = scriptCalls(errAmbiguous, ebsprovider.ErrNotFound) },
			wantErr: awserrors.ErrorServerInternal, wantState: snapshotStateError, wantCalls: 2},
		{name: "ambiguous, then the repeat is already_exists",
			setup:   func(f *pendingFixture) { f.provider.onCreate = scriptCalls(errAmbiguous, ebsprovider.ErrAlreadyExists) },
			wantErr: awserrors.ErrorServerInternal, wantState: snapshotStateError, wantCalls: 2},
		{name: "ambiguous twice",
			setup:   func(f *pendingFixture) { f.provider.onCreate = scriptCalls(errAmbiguous, ebsprovider.ErrUnavailable) },
			wantErr: awserrors.ErrorServerInternal, wantState: snapshotStatePending, wantCalls: 2},
		{name: "a provider answer with no snapshot is ambiguous",
			setup: func(f *pendingFixture) {
				f.provider.onCreate = func(n int, _ ebsprovider.CreateSnapshotRequest) (bool, *ebsprovider.Snapshot, error) {
					return n == 1, nil, nil
				}
			},
			wantState: snapshotStateCompleted, wantCalls: 2, wantHeld: true},
		{name: "the answer cannot be written, so the reaper is left to settle it",
			setup:     func(f *pendingFixture) { f.store.putsBefore = 1 },
			wantState: snapshotStatePending, wantCalls: 1, wantHeld: true},
		{name: "the refusal cannot be written as error, so it stays pending",
			setup: func(f *pendingFixture) {
				f.store.putsBefore = 1
				f.provider.onCreate = scriptCalls(errAmbiguous, ebsprovider.ErrNotFound)
			},
			wantErr: awserrors.ErrorServerInternal, wantState: snapshotStatePending, wantCalls: 2},
		{name: "a refused document that cannot be removed keeps its reference",
			setup: func(f *pendingFixture) {
				f.store.failDelete = true
				f.provider.onCreate = refuseFrom(1, ebsprovider.ErrVolumeInUse)
			},
			wantErr: awserrors.ErrorIncorrectState, wantState: snapshotStatePending, wantCalls: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPendingFixture(t)
			svc := f.node(nodeA)
			if tc.setup != nil {
				tc.setup(f)
			}

			out, err := f.create(svc)

			if tc.wantErr == "" {
				require.NoError(t, err)
				require.NotNil(t, out)
			} else {
				require.Error(t, err, "the caller must never see success without a provider answer")
				code, _, ok := awserrors.ResolveErrorDetail(err)
				require.True(t, ok, "unregistered error: %v", err)
				assert.Equal(t, tc.wantErr, code)
			}
			assert.Equal(t, tc.wantCalls, f.provider.createCalls())

			docs := f.docs()
			if tc.wantState == "" {
				assert.Empty(t, docs, "a refusal of the first call removes the document")
				assert.Empty(t, f.refsOf(pendingVolumeID), "and its reference")
				return
			}
			require.Len(t, docs, 1)
			doc := docs[0]
			assert.Equal(t, tc.wantState, doc.State)
			assert.Equal(t, []string{doc.SnapshotID}, f.refsOf(pendingVolumeID))
			assert.Empty(t, f.claimHolder(doc.SnapshotID), "the claim is released after the last write")
			_, held := f.providerHolds(doc.SnapshotID)
			assert.Equal(t, tc.wantHeld, held)
			if out != nil {
				assert.Equal(t, doc.SnapshotID, aws.StringValue(out.SnapshotId))
				assert.Equal(t, doc.State, aws.StringValue(out.State), "the response reports what the document holds")
			}
			if doc.State == snapshotStateCompleted {
				assert.Equal(t, "100%", doc.Progress)
			}
			if doc.State == snapshotStatePending && held && tc.wantErr == "" && f.store.putsBefore != 0 {
				assert.Equal(t, "memory://snapshot/"+doc.SnapshotID, doc.ProviderHandle, "a pending answer records the provider's handle")
			}
		})
	}
}

// scriptCalls answers call n with errs[n-1], accepting nothing, and passes
// any later call to the memory provider.
func scriptCalls(errs ...error) func(int, ebsprovider.CreateSnapshotRequest) (bool, *ebsprovider.Snapshot, error) {
	return func(n int, _ ebsprovider.CreateSnapshotRequest) (bool, *ebsprovider.Snapshot, error) {
		if n > len(errs) {
			return false, nil, nil
		}
		return true, nil, errs[n-1]
	}
}

// Before the provider is called nothing exists that the provider could hold,
// so each failure there refuses the request and leaves no document.
func TestCreateSnapshot_FailsBeforeTheProviderIsCalled(t *testing.T) {
	cases := []struct {
		name  string
		build func(f *pendingFixture) *SnapshotServiceImpl
	}{
		{name: "the claim cannot be taken", build: func(f *pendingFixture) *SnapshotServiceImpl {
			return f.nodeOver(nodeA, f.unreachableSync())
		}},
		{name: "the reference cannot be written, so the document is removed", build: func(f *pendingFixture) *SnapshotServiceImpl {
			svc := f.node(nodeA)
			svc.snapRefs = f.unreachableRefs()
			return svc
		}},
		{name: "the document cannot be written", build: func(f *pendingFixture) *SnapshotServiceImpl {
			f.store.putsBefore = 0
			return f.node(nodeA)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPendingFixture(t)
			svc := tc.build(f)

			_, err := f.create(svc)

			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
			assert.Zero(t, f.provider.createCalls(), "the provider must not be called without a recorded document")
			assert.Empty(t, f.docs())
			assert.Empty(t, f.refsOf(pendingVolumeID))
		})
	}
}

// A crash after any step of the request leaves either nothing the provider
// holds, or a document the reaper settles once the restarted node has released
// its claims, with the volume reference a crash before it left missing.
func TestCreateSnapshot_CrashAfterEachStepLeavesADocumentTheReaperSettles(t *testing.T) {
	cases := []struct {
		name    string
		step    createStep
		wantDoc bool
		wantRef bool // the reference exists before the reaper runs
	}{
		{"after the claim", stepClaimed, false, false},
		{"after the document", stepRecorded, true, false},
		{"after the reference", stepReferenced, true, true},
		{"after the provider answered", stepAnswered, true, true},
		{"after the document settled", stepSettled, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPendingFixture(t)
			svc := f.node(nodeA)
			var id string
			svc.crashAt = func(step createStep) bool { return step == tc.step }
			f.provider.onCreate = func(_ int, req ebsprovider.CreateSnapshotRequest) (bool, *ebsprovider.Snapshot, error) {
				id = req.SnapshotID
				return false, nil, nil
			}

			_, err := f.create(svc)
			require.ErrorIs(t, err, errCreateCrashed)

			docs := f.docs()
			if !tc.wantDoc {
				assert.Empty(t, docs)
				assert.Empty(t, f.refsOf(pendingVolumeID))
				assert.Zero(t, f.provider.createCalls(), "no provider snapshot may exist without a document")
				return
			}
			require.Len(t, docs, 1)
			doc := docs[0]
			assert.Equal(t, nodeA, f.claimHolder(doc.SnapshotID), "a crashed request leaves its node's claim")
			if tc.wantRef {
				assert.Equal(t, []string{doc.SnapshotID}, f.refsOf(pendingVolumeID))
			} else {
				assert.Empty(t, f.refsOf(pendingVolumeID))
			}

			restarted := f.node(nodeA)
			released, err := restarted.ReleaseNodeClaims(t.Context())
			require.NoError(t, err)
			assert.Equal(t, 1, released, "a crashed request's claim is released whatever its document's state")
			assert.Empty(t, f.claimHolder(doc.SnapshotID))
			f.sweep(f.node(nodeB))

			settled, ok := f.doc(doc.SnapshotID)
			require.True(t, ok)
			assert.Equal(t, snapshotStateCompleted, settled.State)
			assert.Equal(t, []string{doc.SnapshotID}, f.refsOf(pendingVolumeID), "the settled snapshot has exactly one reference")
			if id != "" {
				assert.Equal(t, id, doc.SnapshotID)
			}
			_, held := f.providerHolds(doc.SnapshotID)
			assert.True(t, held)
		})
	}
}

// A provider that accepts the request's call after the request gave up, and
// after the reaper or a refusal, ends with one completed snapshot.
func TestCreateSnapshot_LateAcceptanceEndsWithOneCompletedSnapshot(t *testing.T) {
	cases := []struct {
		name string
		// refusal is what the request's repeat is answered with.
		refusal error
		// reaperFirst runs the reaper before the late acceptance.
		reaperFirst bool
		wantErr     string
	}{
		{name: "accepted after a volume_in_use repeat clears", refusal: ebsprovider.ErrVolumeInUse, wantErr: awserrors.ErrorIncorrectState},
		{name: "accepted after the reaper repeated it", refusal: ebsprovider.ErrUnavailable, reaperFirst: true, wantErr: awserrors.ErrorServerInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPendingFixture(t)
			svc := f.node(nodeA)
			var late ebsprovider.CreateSnapshotRequest
			f.provider.onCreate = func(n int, req ebsprovider.CreateSnapshotRequest) (bool, *ebsprovider.Snapshot, error) {
				switch n {
				case 1:
					late = req
					return true, nil, errAmbiguous
				case 2:
					return true, nil, tc.refusal
				}
				return false, nil, nil
			}

			_, err := f.create(svc)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			doc := f.onlyDoc()
			require.Equal(t, snapshotStatePending, doc.State)

			reaper := f.node(nodeB)
			if tc.reaperFirst {
				f.sweep(reaper)
				_, held := f.providerHolds(doc.SnapshotID)
				require.True(t, held, "the reaper's repeat creates the snapshot the request could not")
			}
			// The request's first call is accepted only now.
			_, err = f.mem.CreateSnapshot(t.Context(), late)
			require.NoError(t, err, "a late call is answered with the snapshot already held")
			f.sweep(reaper)

			settled, ok := f.doc(doc.SnapshotID)
			require.True(t, ok)
			assert.Equal(t, snapshotStateCompleted, settled.State)
			assert.Len(t, f.docs(), 1)
		})
	}
}

// The reaper running between the request's two calls finds the snapshot
// absent. A live request keeps its claim and is not overtaken; a request on a
// node whose heartbeat stopped is, and its repeat meets the reaper's snapshot.
func TestCreateSnapshot_ReaperBetweenTheRequestsTwoCalls(t *testing.T) {
	cases := []struct {
		name            string
		requestNodeLive bool
		wantReaperCalls int
	}{
		{name: "request node live", requestNodeLive: true, wantReaperCalls: 0},
		{name: "request node stale", requestNodeLive: false, wantReaperCalls: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPendingFixture(t)
			svc := f.node(nodeA)
			reaper := f.node(nodeB)
			if !tc.requestNodeLive {
				f.liveness.beat(nodeA, time.Now().Add(-2*clusterv1.LivenessWindow))
			}
			reaperCalls := -1
			f.provider.onCreate = func(n int, _ ebsprovider.CreateSnapshotRequest) (bool, *ebsprovider.Snapshot, error) {
				if n != 1 {
					return false, nil, nil
				}
				before := f.provider.createCalls()
				f.sweep(reaper)
				reaperCalls = f.provider.createCalls() - before
				return true, nil, errAmbiguous
			}

			out, err := f.create(svc)
			require.NoError(t, err)

			assert.Equal(t, tc.wantReaperCalls, reaperCalls)
			doc := f.onlyDoc()
			assert.Equal(t, snapshotStateCompleted, doc.State)
			assert.Equal(t, snapshotStateCompleted, aws.StringValue(out.State))
		})
	}
}

// A provider that settles the snapshot before the request records its pending
// answer still ends completed. The reaper may not write while the request
// holds the claim, so it settles the document on the sweep after.
func TestCreateSnapshot_CompletedBeforeTheRequestWritesItsHandle(t *testing.T) {
	f := newPendingFixture(t)
	svc := f.node(nodeA)
	reaper := f.node(nodeB)
	f.mem.AnswerSnapshotsPending(true)
	svc.crashAt = func(step createStep) bool {
		if step == stepAnswered {
			doc := f.onlyDoc()
			require.NoError(t, f.mem.SettleSnapshot(doc.SnapshotID, ebsprovider.SnapshotStateCompleted))
			f.sweep(reaper)
			assert.Equal(t, snapshotStatePending, f.onlyDoc().State, "the reaper wrote under a claim the request holds")
		}
		return false
	}

	out, err := f.create(svc)
	require.NoError(t, err)
	assert.Equal(t, snapshotStatePending, aws.StringValue(out.State))

	f.sweep(reaper)
	doc := f.onlyDoc()
	assert.Equal(t, snapshotStateCompleted, doc.State)
	assert.Equal(t, "memory://snapshot/"+doc.SnapshotID, doc.ProviderHandle)
}

// A DeleteSnapshot landing while the provider answers is not undone by the
// request's write of that answer.
func TestCreateSnapshot_DeletedWhileTheProviderAnswersIsNotRecreated(t *testing.T) {
	f := newPendingFixture(t)
	svc := f.node(nodeA)
	svc.crashAt = func(step createStep) bool {
		if step == stepAnswered {
			doc := f.onlyDoc()
			require.NoError(t, f.metadata.DeleteSnapshot(t.Context(), testAccountID, doc.SnapshotID))
		}
		return false
	}

	out, err := f.create(svc)
	require.NoError(t, err, "the provider answered, so the request succeeds")

	assert.Equal(t, snapshotStateCompleted, aws.StringValue(out.State))
	assert.Empty(t, f.docs(), "a deleted document must not be written back")
}

// Each row of the reaper's table for a pending document.
func TestPendingSnapshotReaper_SettlesEachRow(t *testing.T) {
	const id = "snap-reaperrow00001"
	stale := time.Now().Add(-2 * clusterv1.LivenessWindow)
	cases := []struct {
		name      string
		setup     func(f *pendingFixture)
		wantState string // empty: the document is gone
		wantCalls int    // CreateSnapshot calls the sweep makes
		wantClaim string // who holds the claim afterwards
		// unreachableClaims builds the reaper over a sync bucket it cannot reach.
		unreachableClaims bool
	}{
		{name: "provider pending", setup: func(f *pendingFixture) {
			f.mem.AnswerSnapshotsPending(true)
			providerSnapshot(f, id)
		}, wantState: snapshotStatePending},
		{name: "provider completed", setup: func(f *pendingFixture) { providerSnapshot(f, id) },
			wantState: snapshotStateCompleted},
		{name: "provider error", setup: func(f *pendingFixture) {
			f.mem.AnswerSnapshotsPending(true)
			providerSnapshot(f, id)
			require.NoError(f.t, f.mem.SettleSnapshot(id, ebsprovider.SnapshotStateError))
		}, wantState: snapshotStateError},
		{name: "provider reports no state", setup: func(f *pendingFixture) {
			f.provider.onList = func(req ebsprovider.ListSnapshotsRequest) (bool, *ebsprovider.ListSnapshotsResponse, error) {
				return true, &ebsprovider.ListSnapshotsResponse{Snapshots: []ebsprovider.SnapshotRef{{ID: req.SnapshotID}}}, nil
			}
		}, wantState: snapshotStatePending},
		{name: "absent, claim held by a live node", setup: func(f *pendingFixture) { f.holdClaim(nodeC, id) },
			wantState: snapshotStatePending, wantClaim: nodeC},
		{name: "absent, claim held by a node whose heartbeat cannot be read", setup: func(f *pendingFixture) {
			f.holdClaim(nodeC, id)
			f.liveness.err[nodeC] = errors.New("cluster state unreadable")
		}, wantState: snapshotStatePending, wantClaim: nodeC},
		{name: "absent, claim held by a stale node is taken over", setup: func(f *pendingFixture) {
			f.holdClaim(nodeC, id)
			f.liveness.beat(nodeC, stale)
		}, wantState: snapshotStateCompleted, wantCalls: 1},
		{name: "absent, claim held by a node with no heartbeat is taken over", setup: func(f *pendingFixture) {
			f.holdClaim("node-gone", id)
		}, wantState: snapshotStateCompleted, wantCalls: 1},
		{name: "absent, claim free, repeat completes", wantState: snapshotStateCompleted, wantCalls: 1},
		{name: "absent, claim free, repeat answers pending", setup: func(f *pendingFixture) { f.mem.AnswerSnapshotsPending(true) },
			wantState: snapshotStatePending, wantCalls: 1},
		{name: "absent, repeat not_found because the source volume is gone", setup: func(f *pendingFixture) {
			require.NoError(f.t, f.mem.DeleteVolume(f.t.Context(), ebsprovider.DeleteVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: pendingVolumeID}))
		}, wantState: snapshotStateError, wantCalls: 1},
		{name: "absent, repeat already_exists", setup: func(f *pendingFixture) { f.provider.onCreate = refuseFrom(1, ebsprovider.ErrAlreadyExists) },
			wantState: snapshotStateError, wantCalls: 1},
		{name: "absent, repeat volume_in_use", setup: func(f *pendingFixture) { f.provider.onCreate = refuseFrom(1, ebsprovider.ErrVolumeInUse) },
			wantState: snapshotStatePending, wantCalls: 1},
		{name: "absent, repeat unavailable", setup: func(f *pendingFixture) { f.provider.onCreate = refuseFrom(1, ebsprovider.ErrUnavailable) },
			wantState: snapshotStatePending, wantCalls: 1},
		{name: "provider read fails", setup: func(f *pendingFixture) {
			f.provider.onList = func(ebsprovider.ListSnapshotsRequest) (bool, *ebsprovider.ListSnapshotsResponse, error) {
				return true, nil, ebsprovider.ErrUnavailable
			}
		}, wantState: snapshotStatePending},
		{name: "deleted between the list and the write", setup: func(f *pendingFixture) {
			providerSnapshot(f, id)
			f.provider.onList = deleteDocFirst(f, id)
		}},
		{name: "deleted between the list and the repeat", setup: func(f *pendingFixture) {
			f.provider.onList = deleteDocFirst(f, id)
		}},
		{name: "the settled state cannot be written", setup: func(f *pendingFixture) {
			providerSnapshot(f, id)
			f.store.putsBefore = 0
		}, wantState: snapshotStatePending},
		{name: "claim store unreachable", unreachableClaims: true, wantState: snapshotStatePending},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPendingFixture(t)
			f.seedPending(id)
			if tc.setup != nil {
				tc.setup(f)
			}
			reaper := f.node(nodeB)
			if tc.unreachableClaims {
				reaper = f.nodeOver(nodeB, f.unreachableSync())
			}
			before := f.provider.createCalls()

			_, err := reaper.NewPendingSnapshotReaper().Sweep(t.Context())
			require.NoError(t, err)

			assert.Equal(t, tc.wantCalls, f.provider.createCalls()-before)
			doc, ok := f.doc(id)
			if tc.wantState == "" {
				assert.False(t, ok, "a deleted document must not be recreated")
			} else {
				require.True(t, ok)
				assert.Equal(t, tc.wantState, doc.State)
			}
			if tc.wantState == snapshotStateCompleted {
				assert.Equal(t, "100%", doc.Progress)
				assert.Equal(t, "memory://snapshot/"+id, doc.ProviderHandle)
			}
			if !tc.unreachableClaims {
				assert.Equal(t, tc.wantClaim, f.claimHolder(id))
			}
		})
	}
}

// The reaper's repeat sends the source volume's handle as the request did, and
// a settle restores the reference a request that crashed after its document
// left missing, removing it again when the document is deleted meanwhile.
func TestPendingSnapshotReaper_RepeatHandleAndReference(t *testing.T) {
	const id = "snap-reaperhandle01"
	volumeHandle := "memory://volume/" + pendingVolumeID
	deleteVolumeDoc := func(f *pendingFixture) {
		require.NoError(f.t, f.metadata.DeleteVolume(f.t.Context(), testAccountID, pendingVolumeID))
	}
	cases := []struct {
		name       string
		setup      func(f *pendingFixture)
		reaper     func(f *pendingFixture) *SnapshotServiceImpl
		wantCalls  int
		wantHandle string
		wantState  string // empty: the document is gone
		wantRefs   []string
	}{
		{name: "the volume document's handle is sent", wantCalls: 1, wantHandle: volumeHandle,
			wantState: snapshotStateCompleted, wantRefs: []string{id}},
		{name: "a gone volume document sends no handle, and the provider holds the volume", setup: deleteVolumeDoc,
			wantCalls: 1, wantState: snapshotStateCompleted, wantRefs: []string{id}},
		{name: "a gone volume document sends no handle, and the provider answers not_found", setup: func(f *pendingFixture) {
			deleteVolumeDoc(f)
			require.NoError(f.t, f.mem.DeleteVolume(f.t.Context(), ebsprovider.DeleteVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: pendingVolumeID}))
		}, wantCalls: 1, wantState: snapshotStateError, wantRefs: []string{id}},
		{name: "an unreadable volume document is retried", setup: func(f *pendingFixture) { f.store.failVolumeGet = true },
			wantState: snapshotStatePending},
		{name: "a reference that cannot be restored leaves the document pending", setup: func(f *pendingFixture) { providerSnapshot(f, id) },
			reaper: func(f *pendingFixture) *SnapshotServiceImpl {
				svc := f.node(nodeB)
				svc.snapRefs = f.unreachableRefs()
				return svc
			}, wantState: snapshotStatePending},
		{name: "a document deleted before the write keeps no restored reference", setup: func(f *pendingFixture) {
			providerSnapshot(f, id)
			f.provider.onList = deleteDocFirst(f, id)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPendingFixture(t)
			f.seedPending(id)
			if tc.setup != nil {
				tc.setup(f)
			}
			var sent []string
			f.provider.onCreate = func(_ int, req ebsprovider.CreateSnapshotRequest) (bool, *ebsprovider.Snapshot, error) {
				sent = append(sent, req.VolumeHandle)
				return false, nil, nil
			}
			reaper := f.node(nodeB)
			if tc.reaper != nil {
				reaper = tc.reaper(f)
			}
			before := f.provider.createCalls()

			f.sweep(reaper)

			assert.Equal(t, tc.wantCalls, f.provider.createCalls()-before)
			if tc.wantCalls > 0 {
				assert.Equal(t, []string{tc.wantHandle}, sent)
			}
			doc, ok := f.doc(id)
			if tc.wantState == "" {
				assert.False(t, ok)
			} else {
				require.True(t, ok)
				assert.Equal(t, tc.wantState, doc.State)
			}
			assert.Equal(t, tc.wantRefs, f.refsOf(pendingVolumeID))
		})
	}
}

// providerSnapshot makes the provider hold id, as an accepted call does.
func providerSnapshot(f *pendingFixture, id string) {
	f.t.Helper()
	_, err := f.mem.CreateSnapshot(f.t.Context(), ebsprovider.CreateSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: id, VolumeID: pendingVolumeID})
	require.NoError(f.t, err)
}

// deleteDocFirst deletes id's document as the reaper reads the provider, as a
// DeleteSnapshot landing mid-sweep does.
func deleteDocFirst(f *pendingFixture, id string) func(ebsprovider.ListSnapshotsRequest) (bool, *ebsprovider.ListSnapshotsResponse, error) {
	return func(ebsprovider.ListSnapshotsRequest) (bool, *ebsprovider.ListSnapshotsResponse, error) {
		require.NoError(f.t, f.metadata.DeleteSnapshot(f.t.Context(), testAccountID, id))
		return false, nil, nil
	}
}

func TestPendingSnapshotReaper_SweepBoundaries(t *testing.T) {
	t.Run("no provider settles nothing", func(t *testing.T) {
		f := newPendingFixture(t)
		f.seedPending("snap-noprovider0001")
		svc := f.node(nodeB)
		svc.provider = nil
		written, err := svc.NewPendingSnapshotReaper().Sweep(t.Context())
		require.NoError(t, err)
		assert.Zero(t, written)
	})
	t.Run("an unreadable document fails the sweep", func(t *testing.T) {
		f := newPendingFixture(t)
		_, err := f.store.PutObject(t.Context(), &s3.PutObjectInput{
			Bucket: aws.String("test-bucket"), Key: aws.String(snapshotDocumentPrefix + testAccountID + "/snap-corrupt000001.json"),
			Body: strings.NewReader("{not json"),
		})
		require.NoError(t, err)
		_, err = f.node(nodeB).NewPendingSnapshotReaper().Sweep(t.Context())
		require.Error(t, err)
		_, err = f.node(nodeA).ReleaseNodeClaims(t.Context())
		require.Error(t, err)
	})
	t.Run("a cancelled sweep stops", func(t *testing.T) {
		f := newPendingFixture(t)
		f.seedPending("snap-cancelled00001")
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := f.node(nodeB).NewPendingSnapshotReaper().Sweep(ctx)
		require.ErrorIs(t, err, context.Canceled)
	})
	t.Run("settled documents are counted and others skipped", func(t *testing.T) {
		f := newPendingFixture(t)
		f.seedPending("snap-counted000001")
		require.NoError(t, f.metadata.PutSnapshot(t.Context(), ebsmetadata.Snapshot{
			SnapshotID: "snap-done000000001", VolumeID: pendingVolumeID, State: snapshotStateCompleted, OwnerID: testAccountID,
		}))
		written, err := f.node(nodeB).NewPendingSnapshotReaper().Sweep(t.Context())
		require.NoError(t, err)
		assert.Equal(t, 1, written)
		assert.Equal(t, 1, f.provider.createCalls(), "only the pending document is asked about")
	})
}

// A restarted daemon frees only its own node's claims, on documents in any
// state, and none a request of the new process holds; until it does, a live node's
// claim holds the reaper off.
func TestReleaseNodeClaims_FreesARestartedNodesClaims(t *testing.T) {
	f := newPendingFixture(t)
	f.seedPending("snap-restartown0001")
	f.seedPending("snap-restartother01")
	f.seedPending("snap-restartlive001")
	require.NoError(t, f.metadata.PutSnapshot(t.Context(), ebsmetadata.Snapshot{
		SnapshotID: "snap-restartdone001", VolumeID: pendingVolumeID, State: snapshotStateCompleted, OwnerID: testAccountID,
	}))
	f.holdClaim(nodeA, "snap-restartown0001")
	f.holdClaim(nodeC, "snap-restartother01")
	f.holdClaim(nodeA, "snap-restartlive001")
	f.holdClaim(nodeA, "snap-restartdone001")

	f.sweep(f.node(nodeB))
	own, _ := f.doc("snap-restartown0001")
	assert.Equal(t, snapshotStatePending, own.State, "a live node's claim holds the reaper off")

	restarted := f.node(nodeA)
	inFlight, err := restarted.claims.lock(t.Context(), "snap-restartlive001")
	require.NoError(t, err)
	released, err := restarted.ReleaseNodeClaims(t.Context())
	require.NoError(t, err)

	assert.Equal(t, 2, released)
	assert.Empty(t, f.claimHolder("snap-restartown0001"))
	assert.Empty(t, f.claimHolder("snap-restartdone001"), "a claim on a settled document is released too")
	assert.Equal(t, nodeC, f.claimHolder("snap-restartother01"), "another node's claim is not this node's to release")
	assert.Equal(t, nodeA, f.claimHolder("snap-restartlive001"), "a claim this process holds is not released")
	inFlight.release(t.Context())

	f.sweep(f.node(nodeB))
	own, _ = f.doc("snap-restartown0001")
	assert.Equal(t, snapshotStateCompleted, own.State)
}

func TestClaims_LocalExclusion(t *testing.T) {
	f := newPendingFixture(t)
	svc := f.node(nodeA)
	held, err := svc.claims.lock(t.Context(), "snap-localheld00001")
	require.NoError(t, err)

	_, err = svc.claims.lock(t.Context(), "snap-localheld00001")
	require.ErrorIs(t, err, errClaimHeld, "a second request of this process is refused")
	_, err = svc.claims.take(t.Context(), "snap-localheld00001")
	require.ErrorIs(t, err, errClaimHeld, "the reaper on this node must not adopt a running request's claim")

	held.release(t.Context())
	again, err := svc.claims.take(t.Context(), "snap-localheld00001")
	require.NoError(t, err)
	again.release(t.Context())

	f.holdClaim(nodeC, "snap-localother0001")
	_, err = svc.claims.lock(t.Context(), "snap-localother0001")
	require.Error(t, err, "a request never takes over")
	noLiveness := NewClaims(NewClaimGroup(kvstore.OpenSyncBucket(f.js), nodeB), nodeB, nil)
	_, err = noLiveness.take(t.Context(), "snap-localother0001")
	require.ErrorIs(t, err, errClaimHeld, "with no liveness wired no holder is judged dead")

	local := localClaims()
	h, err := local.take(t.Context(), "snap-localonly00001")
	require.NoError(t, err)
	_, err = local.lock(t.Context(), "snap-localonly00001")
	require.ErrorIs(t, err, errClaimHeld)
	h.release(t.Context())
	released, err := local.releaseOwn(t.Context(), "snap-localonly00001")
	require.NoError(t, err)
	assert.False(t, released)
}

// Verifies: ADR-0003:S7.
//
// Two sweeps over every unmarked row, beside marked documents and a tombstone,
// delete only the marked snapshots and their documents, and no volume or AMI.
func TestS7_PendingSnapshotReaperDeletesOnlyWhatACustomerDeleted(t *testing.T) {
	f := newPendingFixture(t)
	const otherVolume = "vol-pendingother001"
	_, err := f.mem.CreateVolume(t.Context(), ebsprovider.CreateVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: otherVolume, CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
	})
	require.NoError(t, err)
	amis := []string{"ami-0123456789abcdef0", "ami-0123456789abcdef1"}
	require.NoError(t, f.metadata.PutAMI(t.Context(), ebsmetadata.AMI{ImageID: amis[0], SnapshotID: "snap-s7completed001", State: "pending"}))
	require.NoError(t, f.metadata.PutAMI(t.Context(), ebsmetadata.AMI{ImageID: amis[1], SnapshotID: "snap-s7neverexists1", State: "pending"}))

	ids := map[string]func(){
		"snap-s7pending00001": func() {
			f.mem.AnswerSnapshotsPending(true)
			providerSnapshot(f, "snap-s7pending00001")
			f.mem.AnswerSnapshotsPending(false)
		},
		"snap-s7completed001": func() { providerSnapshot(f, "snap-s7completed001") },
		"snap-s7error0000001": func() {
			f.mem.AnswerSnapshotsPending(true)
			providerSnapshot(f, "snap-s7error0000001")
			f.mem.AnswerSnapshotsPending(false)
			require.NoError(t, f.mem.SettleSnapshot("snap-s7error0000001", ebsprovider.SnapshotStateError))
		},
		"snap-s7heldlive0001": func() { f.holdClaim(nodeC, "snap-s7heldlive0001") },
		"snap-s7heldstale001": func() { f.holdClaim("node-gone", "snap-s7heldstale001") },
		"snap-s7absent000001": func() {},
		"snap-s7volumegone01": func() {},
		"snap-s7elsewhere001": func() {},
		"snap-s7inuse0000001": func() {},
		"snap-s7readfails001": func() {},
	}
	for id, setup := range ids {
		f.seedPending(id)
		setup()
	}
	require.NoError(t, f.metadata.PutSnapshot(t.Context(), ebsmetadata.Snapshot{
		SnapshotID: "snap-s7volumegone01", VolumeID: "vol-neverexisted01", State: snapshotStatePending, OwnerID: testAccountID,
	}))
	// Settled documents the reaper must leave alone, one with a create unanswered.
	for id, ambiguous := range map[string]bool{"snap-s7settled00001": false, "snap-s7settledamb01": true} {
		require.NoError(t, f.metadata.PutSnapshot(t.Context(), ebsmetadata.Snapshot{
			SnapshotID: id, VolumeID: pendingVolumeID, State: snapshotStateCompleted, OwnerID: testAccountID, AmbiguousCreate: ambiguous,
		}))
		providerSnapshot(f, id)
		ids[id] = func() {}
	}
	marked := map[string]ebsmetadata.Snapshot{
		"snap-s7markeddone01": {State: snapshotStatePending},
		"snap-s7markedgone01": {State: snapshotStatePending},
		"snap-s7markedtomb01": {State: snapshotStateCompleted, AmbiguousCreate: true},
		"snap-s7tombstone001": {State: snapshotStateCompleted, AmbiguousCreate: true, TombstonedAt: time.Now()},
	}
	for id, doc := range marked {
		doc.SnapshotID, doc.VolumeID, doc.OwnerID, doc.DeleteRequested = id, pendingVolumeID, testAccountID, true
		require.NoError(t, f.metadata.PutSnapshot(t.Context(), doc))
	}
	providerSnapshot(f, "snap-s7markeddone01")
	providerSnapshot(f, "snap-s7markedtomb01")
	providerSnapshot(f, "snap-s7tombstone001")

	f.provider.onCreate = func(_ int, req ebsprovider.CreateSnapshotRequest) (bool, *ebsprovider.Snapshot, error) {
		switch req.SnapshotID {
		case "snap-s7elsewhere001":
			return true, nil, ebsprovider.ErrAlreadyExists
		case "snap-s7inuse0000001":
			return true, nil, ebsprovider.ErrVolumeInUse
		}
		return false, nil, nil
	}
	f.provider.onList = func(req ebsprovider.ListSnapshotsRequest) (bool, *ebsprovider.ListSnapshotsResponse, error) {
		if req.SnapshotID == "snap-s7readfails001" {
			return true, nil, ebsprovider.ErrUnavailable
		}
		return false, nil, nil
	}

	reaper := f.node(nodeB)
	f.sweep(reaper)
	f.sweep(reaper)

	asked := map[string]bool{}
	for id := range marked {
		asked["snapshot "+id] = true
		asked[snapshotDocumentPrefix+testAccountID+"/"+id+".json"] = true
	}
	for _, del := range f.provider.deleteCalls() {
		assert.True(t, asked[del], "ADR-0003:S7: the pending-snapshot reaper asked the provider to delete %s, which no customer deleted", del)
	}
	f.store.mu.Lock()
	storeDeletes := append([]string(nil), f.store.deletes...)
	f.store.mu.Unlock()
	for _, key := range storeDeletes {
		assert.True(t, asked[key], "ADR-0003:S7: the pending-snapshot reaper deleted %s, which no customer deleted", key)
	}
	for id := range ids {
		_, ok := f.doc(id)
		assert.True(t, ok, "ADR-0003:S7: snapshot document %s is gone after the sweep", id)
	}
	for _, id := range []string{"snap-s7completed001", "snap-s7settled00001", "snap-s7settledamb01"} {
		_, held := f.providerHolds(id)
		assert.True(t, held, "ADR-0003:S7: snapshot %s, which no customer deleted, is gone at the provider", id)
	}
	for _, id := range amis {
		_, err = f.metadata.GetAMI(t.Context(), id)
		assert.NoError(t, err, "ADR-0003:S7: the AMI document %s is gone after the sweep", id)
	}
	_, err = f.mem.GetVolume(t.Context(), ebsprovider.GetVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: pendingVolumeID})
	assert.NoError(t, err, "ADR-0003:S7: the source volume is gone after the sweep")
	volumes, err := f.metadata.ListAllVolumes(t.Context())
	require.NoError(t, err)
	assert.Len(t, volumes, 1, "ADR-0003:S7: a volume document is gone after the sweep")
	_, ok := f.doc("snap-s7markeddone01")
	assert.False(t, ok, "a snapshot the customer deleted is deleted once it settles")
}
