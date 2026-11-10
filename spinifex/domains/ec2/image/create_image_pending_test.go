package image

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	ebsmetadata "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	ec2snapshot "github.com/mulgadc/spinifex/spinifex/domains/ec2/snapshot"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	ebsprovider "github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pendingImageFixture is an image service wired to a snapshot service over the
// same store and memory provider, whose reaper settles what CreateImage leaves.
type pendingImageFixture struct {
	svc       *ImageServiceImpl
	snapshots *ec2snapshot.SnapshotServiceImpl
	mem       *ebsprovider.MemoryProvider
	volume    ebsmetadata.Volume
}

func newPendingImageFixture(t *testing.T) *pendingImageFixture {
	t.Helper()
	svc, _, provider := setupProviderSnapshotImageService(t)
	mem, ok := provider.(*ebsprovider.MemoryProvider)
	require.True(t, ok)
	snapshots := useProvider(svc, mem)
	return &pendingImageFixture{
		svc:       svc,
		snapshots: snapshots,
		mem:       mem,
		volume:    seedProviderVolume(t, svc, provider, "vol-createimage01", 8),
	}
}

func (f *pendingImageFixture) createImage(t *testing.T) (*ec2.CreateImageOutput, error) {
	t.Helper()
	return f.svc.CreateImageFromInstance(CreateImageParams{
		Input:        &ec2.CreateImageInput{InstanceId: aws.String("i-createimage01"), Name: aws.String("pending-image")},
		RootVolumeID: f.volume.VolumeID,
	}, testAccountID)
}

// onlyAMI is the one AMI document in the store, and its snapshot's document.
func (f *pendingImageFixture) onlyAMI(t *testing.T) (ebsmetadata.AMI, ebsmetadata.Snapshot) {
	t.Helper()
	amis, err := f.svc.MetadataStore().ListAMIs(context.Background())
	require.NoError(t, err)
	require.Len(t, amis, 1)
	snap, err := f.svc.MetadataStore().GetSnapshot(context.Background(), testAccountID, amis[0].SnapshotID)
	require.NoError(t, err)
	return amis[0], snap
}

func (f *pendingImageFixture) sweep(t *testing.T) {
	t.Helper()
	_, err := f.snapshots.NewPendingSnapshotReaper().Sweep(context.Background())
	require.NoError(t, err)
}

// recordingCreateProvider reads the image's documents when the provider is called.
type recordingCreateProvider struct {
	ebsprovider.EBSProvider

	store     *ebsmetadata.Store
	amis      []ebsmetadata.AMI
	snapshot  ebsmetadata.Snapshot
	readError error
}

func (p *recordingCreateProvider) CreateSnapshot(ctx context.Context, req ebsprovider.CreateSnapshotRequest) (*ebsprovider.Snapshot, error) {
	p.amis, p.readError = p.store.ListAMIs(ctx)
	if p.readError == nil {
		p.snapshot, p.readError = p.store.GetSnapshot(ctx, testAccountID, req.SnapshotID)
	}
	return p.EBSProvider.CreateSnapshot(ctx, req)
}

func TestCreateImage_WritesBothDocumentsPendingBeforeTheProviderCall(t *testing.T) {
	f := newPendingImageFixture(t)
	recording := &recordingCreateProvider{EBSProvider: f.mem, store: f.svc.MetadataStore()}
	f.snapshots.SetEBSProvider(recording)

	_, err := f.createImage(t)
	require.NoError(t, err)

	require.NoError(t, recording.readError)
	require.Len(t, recording.amis, 1, "the AMI document exists when the provider is called")
	assert.Equal(t, ebsmetadata.AMIStatePending, recording.amis[0].State)
	assert.Equal(t, recording.snapshot.SnapshotID, recording.amis[0].SnapshotID)
	assert.Equal(t, "pending", recording.snapshot.State, "the snapshot document is pending when the provider is called")
}

func TestCreateImage_DefiniteRefusalRemovesBothDocuments(t *testing.T) {
	f := newPendingImageFixture(t)
	f.snapshots.SetEBSProvider(&createInUseProvider{EBSProvider: f.mem})

	_, err := f.createImage(t)
	code, _, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok, "unregistered error: %v", err)
	assert.Equal(t, awserrors.ErrorIncorrectState, code)

	amis, err := f.svc.MetadataStore().ListAMIs(context.Background())
	require.NoError(t, err)
	assert.Empty(t, amis, "a refused image leaves no AMI document")
	snaps, err := f.svc.MetadataStore().ListAllSnapshots(context.Background())
	require.NoError(t, err)
	assert.Empty(t, snaps, "a refused image leaves no snapshot document")
}

func TestCreateImage_SettlesTheSnapshotAndTheAMI(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(f *pendingImageFixture)
		wantErr string
		// settle is how the provider finishes a snapshot it answered pending; empty: it did not.
		settle      ebsprovider.SnapshotState
		wantPending bool
		wantSnap    string
		wantAMI     string
	}{
		{name: "completed in the request", arrange: func(*pendingImageFixture) {}, wantSnap: "completed", wantAMI: ebsmetadata.AMIStateAvailable},
		{
			name: "answered pending, then completed", arrange: func(f *pendingImageFixture) { f.mem.AnswerSnapshotsPending(true) },
			wantPending: true, settle: ebsprovider.SnapshotStateCompleted, wantSnap: "completed", wantAMI: ebsmetadata.AMIStateAvailable,
		},
		{
			name: "answered pending, then error", arrange: func(f *pendingImageFixture) { f.mem.AnswerSnapshotsPending(true) },
			wantPending: true, settle: ebsprovider.SnapshotStateError, wantSnap: "error", wantAMI: ebsmetadata.AMIStateFailed,
		},
		{
			name: "both answers lost", arrange: func(f *pendingImageFixture) { f.mem.FailCreateSnapshotsAfterAccept(2, nil) },
			wantErr: awserrors.ErrorServerInternal, wantPending: true, wantSnap: "completed", wantAMI: ebsmetadata.AMIStateAvailable,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newPendingImageFixture(t)
			tc.arrange(f)

			_, err := f.createImage(t)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}

			ami, snap := f.onlyAMI(t)
			if tc.wantPending {
				assert.Equal(t, ebsmetadata.AMIStatePending, ami.State)
				assert.Equal(t, "pending", snap.State)
				if tc.settle != "" {
					require.NoError(t, f.mem.SettleSnapshot(snap.SnapshotID, tc.settle))
				}
				f.sweep(t)
				ami, snap = f.onlyAMI(t)
			}
			assert.Equal(t, tc.wantSnap, snap.State)
			assert.Equal(t, tc.wantAMI, ami.State)
		})
	}
}

// DeregisterImage is allowed on an image whose snapshot has not completed.
func TestDeregisterImage_AllowsAPendingAMI(t *testing.T) {
	f := newPendingImageFixture(t)
	f.mem.AnswerSnapshotsPending(true)
	out, err := f.createImage(t)
	require.NoError(t, err)

	_, err = f.svc.DeregisterImage(context.Background(), &ec2.DeregisterImageInput{ImageId: out.ImageId}, testAccountID)
	require.NoError(t, err)
	_, err = f.svc.MetadataStore().GetAMI(context.Background(), aws.StringValue(out.ImageId))
	assert.Error(t, err, "the pending AMI is deregistered")
}

// putHookStore runs onPut once, before the write to a matching key that
// follows skip earlier matching writes reaches the store.
type putHookStore struct {
	objectstore.ObjectStore

	match func(key string) bool
	skip  int
	onPut func()

	mu   sync.Mutex
	seen int
}

func (s *putHookStore) PutObject(ctx context.Context, input *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
	if s.match(aws.StringValue(input.Key)) {
		s.mu.Lock()
		fire := s.seen == s.skip
		s.seen++
		s.mu.Unlock()
		if fire {
			s.onPut()
		}
	}
	return s.ObjectStore.PutObject(ctx, input)
}

// A DeregisterImage that lands between the reaper's reread of a pending AMI
// and its write either waits the write out or is refused retryably; one that
// succeeds is never undone by the reaper writing the settled AMI back.
func TestDeregisterImage_DuringTheReapersAMIWriteStaysDeleted(t *testing.T) {
	f := newPendingImageFixture(t)
	f.mem.AnswerSnapshotsPending(true)
	out, err := f.createImage(t)
	require.NoError(t, err)
	imageID := aws.StringValue(out.ImageId)
	ami, snap := f.onlyAMI(t)
	require.Equal(t, ebsmetadata.AMIStatePending, ami.State)
	require.NoError(t, f.mem.SettleSnapshot(snap.SnapshotID, ebsprovider.SnapshotStateCompleted))

	key, err := ebsmetadata.AMIKey(imageID)
	require.NoError(t, err)
	var deregErr error
	deregistered := false
	hooked := &putHookStore{ObjectStore: f.svc.store, match: func(k string) bool { return k == key }}
	hooked.onPut = func() {
		deregistered = true
		_, deregErr = f.svc.DeregisterImage(context.Background(), &ec2.DeregisterImageInput{ImageId: out.ImageId}, testAccountID)
	}
	reaping := ec2snapshot.NewSnapshotServiceImplWithStore(f.svc.config, hooked, nil)
	reaping.SetEBSProvider(f.mem)
	f.svc.SetImageClaims(reaping)

	_, err = reaping.NewPendingSnapshotReaper().Sweep(context.Background())
	require.NoError(t, err)
	require.True(t, deregistered, "the reaper wrote the settled AMI")

	_, getErr := f.svc.MetadataStore().GetAMI(context.Background(), imageID)
	if deregErr == nil {
		require.True(t, objectstore.IsNoSuchKeyError(getErr), "a deregistered AMI was written back: %v", getErr)
		return
	}
	code, _, ok := awserrors.ResolveErrorDetail(deregErr)
	require.True(t, ok, "unregistered error: %v", deregErr)
	assert.Equal(t, awserrors.ErrorIncorrectState, code, "a refused deregistration is retryable")
	require.NoError(t, getErr)

	_, err = f.svc.DeregisterImage(context.Background(), &ec2.DeregisterImageInput{ImageId: out.ImageId}, testAccountID)
	require.NoError(t, err, "the retry succeeds once the reaper's write is done")
	_, err = reaping.NewPendingSnapshotReaper().Sweep(context.Background())
	require.NoError(t, err)
	_, getErr = f.svc.MetadataStore().GetAMI(context.Background(), imageID)
	assert.True(t, objectstore.IsNoSuchKeyError(getErr), "the retried deregistration stays deleted: %v", getErr)
}

// A DeregisterImage that lands between CreateImage's reread of its pending AMI
// and the write making it available is refused retryably, never undone.
func TestDeregisterImage_DuringCreateImagesAMIWriteStaysDeleted(t *testing.T) {
	f := newPendingImageFixture(t)
	sample, err := ebsmetadata.AMIKey("ami-0")
	require.NoError(t, err)
	amiKeyPrefix := sample[:strings.LastIndex(sample, "ami-0")]
	var imageID string
	var deregErr error
	deregistered := false
	// The Recorded hook's pending write is skipped; the Completed hook's is the one.
	hooked := &putHookStore{
		ObjectStore: f.svc.store,
		skip:        1,
		match:       func(k string) bool { return strings.HasPrefix(k, amiKeyPrefix) },
	}
	hooked.onPut = func() {
		amis, err := f.svc.MetadataStore().ListAMIs(context.Background())
		if err != nil || len(amis) != 1 {
			deregErr = err
			return
		}
		imageID = amis[0].ImageID
		deregistered = true
		_, deregErr = f.svc.DeregisterImage(context.Background(), &ec2.DeregisterImageInput{ImageId: aws.String(imageID)}, testAccountID)
	}
	f.svc.metadata = ebsmetadata.NewStore(hooked, testBucket)
	settling := ec2snapshot.NewSnapshotServiceImplWithStore(f.svc.config, hooked, nil)
	settling.SetEBSProvider(f.mem)
	f.svc.SetSnapshotCreator(settling)
	f.svc.SetImageClaims(settling)

	out, err := f.createImage(t)
	require.NoError(t, err)
	require.True(t, deregistered, "CreateImage wrote the available AMI")
	require.Equal(t, imageID, aws.StringValue(out.ImageId))

	_, getErr := f.svc.MetadataStore().GetAMI(context.Background(), imageID)
	if deregErr == nil {
		require.True(t, objectstore.IsNoSuchKeyError(getErr), "a deregistered AMI was written back: %v", getErr)
		return
	}
	code, _, ok := awserrors.ResolveErrorDetail(deregErr)
	require.True(t, ok, "unregistered error: %v", deregErr)
	assert.Equal(t, awserrors.ErrorIncorrectState, code, "a refused deregistration is retryable")
	require.NoError(t, getErr)

	_, err = f.svc.DeregisterImage(context.Background(), &ec2.DeregisterImageInput{ImageId: out.ImageId}, testAccountID)
	require.NoError(t, err, "the retry succeeds once CreateImage's write is done")
	f.sweep(t)
	_, getErr = f.svc.MetadataStore().GetAMI(context.Background(), imageID)
	assert.True(t, objectstore.IsNoSuchKeyError(getErr), "the retried deregistration stays deleted: %v", getErr)
}
