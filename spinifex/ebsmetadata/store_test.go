package ebsmetadata

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStoreRoundTripsVolumeAndAMI(t *testing.T) {
	objects := objectstore.NewMemoryObjectStore()
	store := NewStore(objects, "control-plane")

	volume := Volume{VolumeID: "vol-1", TenantID: "000000000001", CapacityGiB: 10, State: "available", ProviderHandle: "opaque"}
	require.NoError(t, store.PutVolume(context.Background(), volume))
	gotVolume, err := store.GetVolume(context.Background(), volume.TenantID, volume.VolumeID)
	require.NoError(t, err)
	assert.Equal(t, volume.VolumeID, gotVolume.VolumeID)
	assert.Equal(t, volume.ProviderHandle, gotVolume.ProviderHandle)
	assert.Equal(t, SchemaVersion, gotVolume.SchemaVersion)

	ami := AMI{ImageID: "ami-1", Name: "test", SnapshotID: "snap-1"}
	require.NoError(t, store.PutAMI(context.Background(), ami))
	gotAMI, err := store.GetAMI(context.Background(), ami.ImageID)
	require.NoError(t, err)
	assert.Equal(t, ami.ImageID, gotAMI.ImageID)
	assert.Equal(t, ami.SnapshotID, gotAMI.SnapshotID)
	assert.Equal(t, SchemaVersion, gotAMI.SchemaVersion)

	require.NoError(t, store.DeleteVolume(context.Background(), volume.TenantID, volume.VolumeID))
	require.NoError(t, store.DeleteAMI(context.Background(), ami.ImageID))
}

func TestListVolumes_Empty(t *testing.T) {
	store := NewStore(objectstore.NewMemoryObjectStore(), "control-plane")
	volumes, err := store.ListVolumes(context.Background())
	require.NoError(t, err)
	assert.Empty(t, volumes)
}

func TestListVolumes_ReturnsAllStoredVolumes(t *testing.T) {
	store := NewStore(objectstore.NewMemoryObjectStore(), "control-plane")
	ctx := context.Background()

	want := []Volume{
		{VolumeID: "vol-1", TenantID: "000000000001", CapacityGiB: 8, State: "available"},
		{VolumeID: "vol-2", TenantID: "000000000002", CapacityGiB: 16, State: "in-use"},
		{VolumeID: "vol-3", TenantID: "000000000001", CapacityGiB: 32, State: "available"},
	}
	for _, v := range want {
		require.NoError(t, store.PutVolume(ctx, v))
	}

	got, err := store.ListVolumes(ctx)
	require.NoError(t, err)
	require.Len(t, got, len(want))

	gotIDs := make(map[string]uint64, len(got))
	for _, v := range got {
		gotIDs[v.VolumeID] = v.CapacityGiB
	}
	for _, v := range want {
		assert.Equal(t, v.CapacityGiB, gotIDs[v.VolumeID])
	}
}

// corruptVolumeStore returns a store holding one good volume and one object
// under the volumes prefix that is not a valid Volume record.
func corruptVolumeStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	objects := objectstore.NewMemoryObjectStore()
	store := NewStore(objects, "control-plane")
	ctx := context.Background()

	require.NoError(t, store.PutVolume(ctx, Volume{VolumeID: "vol-good", TenantID: "000000000001", CapacityGiB: 1}))

	key, err := VolumeKey("000000000001", "vol-corrupt")
	require.NoError(t, err)
	_, err = objects.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("control-plane"), Key: aws.String(key), Body: bytes.NewReader([]byte("not json")),
	})
	require.NoError(t, err)
	return store, ctx
}

// One undecodable document must not hide every other volume in the cluster:
// a single bad object under the prefix used to fail the whole listing, which
// is how one volume made DescribeVolumes fail for every account.
func TestListVolumes_SkipsCorruptObject(t *testing.T) {
	store, ctx := corruptVolumeStore(t)

	volumes, err := store.ListVolumes(ctx)
	require.NoError(t, err)
	require.Len(t, volumes, 1, "the readable volume must still be listed")
	assert.Equal(t, "vol-good", volumes[0].VolumeID)
}

// The strict listing keeps the old contract, for callers whose answer would be
// wrong rather than merely partial.
func TestListVolumesStrict_CorruptObjectReturnsError(t *testing.T) {
	store, ctx := corruptVolumeStore(t)

	_, err := store.ListVolumesStrict(ctx)
	require.Error(t, err)
}

// A document that cannot be fetched at all is as unusable as one that cannot
// be decoded, and was the shape that took DescribeVolumes down: the object
// existed, was listed, and every read of it failed.
func TestListVolumes_SkipsUnreadableObject(t *testing.T) {
	objects := objectstore.NewMemoryObjectStore()
	store := NewStore(objects, "control-plane")
	ctx := context.Background()

	require.NoError(t, store.PutVolume(ctx, Volume{VolumeID: "vol-good", TenantID: "000000000001", CapacityGiB: 1}))
	key, err := VolumeKey("000000000001", "vol-unreadable")
	require.NoError(t, err)
	require.NoError(t, store.PutVolume(ctx, Volume{VolumeID: "vol-unreadable", TenantID: "000000000001", CapacityGiB: 1}))

	failing := &getFailsStore{ObjectStore: objects, failKey: key}
	broken := NewStore(failing, "control-plane")

	volumes, err := broken.ListVolumes(ctx)
	require.NoError(t, err, "an unreadable document must not fail the whole listing")
	require.Len(t, volumes, 1)
	assert.Equal(t, "vol-good", volumes[0].VolumeID)

	_, err = broken.ListVolumesStrict(ctx)
	require.Error(t, err, "the strict listing must still report the failure")
}

// getFailsStore fails GetObject for one key, standing in for an object whose
// shards no longer reconstruct.
type getFailsStore struct {
	objectstore.ObjectStore

	failKey string
}

func (s *getFailsStore) GetObject(ctx context.Context, in *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	if in.Key != nil && *in.Key == s.failKey {
		return nil, errors.New("reconstruction failed")
	}
	return s.ObjectStore.GetObject(ctx, in)
}

// TestListVolumes_NotConfigured covers the nil-store guard shared by every
// Store method: a zero-value *Store (no ObjectStore wired up) must error
// instead of panicking on a nil dereference.
func TestListVolumes_NotConfigured(t *testing.T) {
	var store *Store
	_, err := store.ListVolumes(context.Background())
	require.Error(t, err)

	empty := &Store{}
	_, err = empty.ListVolumes(context.Background())
	require.Error(t, err)
}

// TestListAMIs_Empty mirrors TestListVolumes_Empty: an AMI-less store lists
// as an empty slice, not an error.
func TestListAMIs_Empty(t *testing.T) {
	store := NewStore(objectstore.NewMemoryObjectStore(), "control-plane")
	amis, err := store.ListAMIs(context.Background())
	require.NoError(t, err)
	assert.Empty(t, amis)
}

// TestListAMIs_ReturnsAllStoredAMIs mirrors TestListVolumes_ReturnsAllStoredVolumes.
func TestListAMIs_ReturnsAllStoredAMIs(t *testing.T) {
	store := NewStore(objectstore.NewMemoryObjectStore(), "control-plane")
	ctx := context.Background()

	want := []AMI{
		{ImageID: "ami-1", Name: "one", VolumeSizeGiB: 8},
		{ImageID: "ami-2", Name: "two", VolumeSizeGiB: 16},
	}
	for _, a := range want {
		require.NoError(t, store.PutAMI(ctx, a))
	}

	got, err := store.ListAMIs(ctx)
	require.NoError(t, err)
	require.Len(t, got, len(want))

	gotSizes := make(map[string]uint64, len(got))
	for _, a := range got {
		gotSizes[a.ImageID] = a.VolumeSizeGiB
	}
	for _, a := range want {
		assert.Equal(t, a.VolumeSizeGiB, gotSizes[a.ImageID])
	}
}

// TestListAMIs_NotConfigured mirrors TestListVolumes_NotConfigured.
func TestListAMIs_NotConfigured(t *testing.T) {
	var store *Store
	_, err := store.ListAMIs(context.Background())
	require.Error(t, err)

	empty := &Store{}
	_, err = empty.ListAMIs(context.Background())
	require.Error(t, err)
}

// --- Legacy fallback tests ---
// TestGetVolume_MissingDocumentIsNotFound locks the only answer a volume with
// no document gets: it does not exist as far as the control plane is
// concerned, reported as the object store's not-found rather than a zero value.
func TestGetVolume_MissingDocumentIsNotFound(t *testing.T) {
	store := NewStore(objectstore.NewMemoryObjectStore(), "control-plane")
	_, err := store.GetVolume(context.Background(), "000000000001", "vol-missing")
	require.Error(t, err)
	assert.True(t, objectstore.IsNoSuchKeyError(err))
}

// TestGetAMI_MissingDocumentIsNotFound mirrors TestGetVolume_MissingDocumentIsNotFound.
func TestGetAMI_MissingDocumentIsNotFound(t *testing.T) {
	store := NewStore(objectstore.NewMemoryObjectStore(), "control-plane")
	_, err := store.GetAMI(context.Background(), "ami-missing")
	require.Error(t, err)
	assert.True(t, objectstore.IsNoSuchKeyError(err))
}

// TestGet_CorruptDocumentIsDistinguishable is what lets the admin tooling tell
// salvage from not-found: an undecodable document must not read as absent, or
// a --force removal would refuse the one case it exists for.
func TestGet_CorruptDocumentIsDistinguishable(t *testing.T) {
	objects := objectstore.NewMemoryObjectStore()
	store := NewStore(objects, "control-plane")
	ctx := context.Background()

	volKey, err := VolumeKey("000000000001", "vol-corrupt")
	require.NoError(t, err)
	writeRaw(t, objects, volKey, []byte("{not json"))
	amiKey, err := AMIKey("ami-corrupt")
	require.NoError(t, err)
	writeRaw(t, objects, amiKey, []byte("{not json"))

	_, err = store.GetVolume(ctx, "000000000001", "vol-corrupt")
	require.ErrorIs(t, err, ErrCorruptDocument)
	assert.False(t, objectstore.IsNoSuchKeyError(err), "corrupt must not read as absent")

	_, err = store.GetAMI(ctx, "ami-corrupt")
	require.ErrorIs(t, err, ErrCorruptDocument)
	assert.False(t, objectstore.IsNoSuchKeyError(err), "corrupt must not read as absent")
}

// writeRaw puts bytes at key without going through Marshal, so a test can
// store a document the store cannot decode.
func writeRaw(t *testing.T, objects objectstore.ObjectStore, key string, data []byte) {
	t.Helper()
	_, err := objects.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String("control-plane"), Key: aws.String(key), Body: bytes.NewReader(data),
	})
	require.NoError(t, err)
}

// TestListAMIs_SkipsCorruptButStrictDoesNot pins the blast radius of one bad
// document: DescribeImages keeps working and loses only the unreadable image,
// while a caller that cannot answer partially still gets the error.
func TestListAMIs_SkipsCorruptButStrictDoesNot(t *testing.T) {
	objects := objectstore.NewMemoryObjectStore()
	store := NewStore(objects, "control-plane")
	ctx := context.Background()

	require.NoError(t, store.PutAMI(ctx, AMI{ImageID: "ami-good", Name: "readable"}))
	badKey, err := AMIKey("ami-bad")
	require.NoError(t, err)
	writeRaw(t, objects, badKey, []byte("{not json"))

	got, err := store.ListAMIs(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1, "the readable AMI must survive its neighbour being corrupt")
	assert.Equal(t, "ami-good", got[0].ImageID)

	_, err = store.ListAMIsStrict(ctx)
	require.ErrorIs(t, err, ErrCorruptDocument)
}

// TestListVolumes_FetchesDocumentsConcurrently is the reason listDocuments has
// a worker pool at all. A listing costs one object fetch per document, and
// fetching them one at a time makes DescribeVolumes scale with the number of
// volumes in the cluster rather than with the number the caller owns.
func TestListVolumes_FetchesDocumentsConcurrently(t *testing.T) {
	objects := objectstore.NewMemoryObjectStore()
	store := NewStore(objects, "control-plane")
	ctx := context.Background()

	const documents = 32
	const delay = 20 * time.Millisecond
	for i := range documents {
		require.NoError(t, store.PutVolume(ctx, Volume{
			VolumeID: fmt.Sprintf("vol-%02d", i), TenantID: "000000000001", CapacityGiB: 1,
		}))
	}

	slow := &slowGetStore{ObjectStore: objects, delay: delay}
	measured := NewStore(slow, "control-plane")

	start := time.Now()
	volumes, err := measured.ListVolumes(ctx)
	elapsed := time.Since(start)

	require.NoError(t, err)
	require.Len(t, volumes, documents)

	serial := documents * delay
	assert.Less(t, elapsed, serial/2,
		"fetching %d documents took %s; serially it would be %s", documents, elapsed, serial)
	assert.Equal(t, int64(documents), slow.gets.Load(), "every document must still be fetched")
	assert.LessOrEqual(t, slow.peak(), int64(listFetchConcurrency),
		"the pool bound must hold: an unbounded fan-out over a large bucket is its own problem")
}

// TestListVolumes_ConcurrentFetchPreservesListingOrder pins the ordering the
// concurrent fetch has to reproduce: whatever order the listing came back in.
// Strict callers depend on it, because it decides which of several unusable
// documents is the one whose error is returned.
func TestListVolumes_ConcurrentFetchPreservesListingOrder(t *testing.T) {
	objects := objectstore.NewMemoryObjectStore()
	store := NewStore(objects, "control-plane")
	ctx := context.Background()

	const documents = 24
	want := make([]string, 0, documents)
	keys := make([]string, 0, documents)
	for i := range documents {
		id := fmt.Sprintf("vol-%02d", i)
		require.NoError(t, store.PutVolume(ctx, Volume{VolumeID: id, TenantID: "000000000001", CapacityGiB: 1}))
		key, err := VolumeKey("000000000001", id)
		require.NoError(t, err)
		want = append(want, id)
		keys = append(keys, key)
	}

	fixed := NewStore(&fixedOrderStore{ObjectStore: objects, keys: keys}, "control-plane")
	for range 5 {
		volumes, err := fixed.ListVolumes(ctx)
		require.NoError(t, err)
		got := make([]string, 0, len(volumes))
		for _, v := range volumes {
			got = append(got, v.VolumeID)
		}
		assert.Equal(t, want, got, "documents must follow the order the listing returned")
	}
}

// fixedOrderStore lists a known set of keys in a known order, because the
// in-memory store lists in map order and cannot pin an ordering guarantee.
type fixedOrderStore struct {
	objectstore.ObjectStore

	keys []string
}

func (s *fixedOrderStore) ListObjectsV2(_ context.Context, _ *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
	contents := make([]*s3.Object, 0, len(s.keys))
	for _, key := range s.keys {
		contents = append(contents, &s3.Object{Key: aws.String(key)})
	}
	return &s3.ListObjectsV2Output{Contents: contents}, nil
}

// slowGetStore delays every fetch and records how many ran at once, so a test
// can tell a concurrent listing from a serial one and check the pool bound.
type slowGetStore struct {
	objectstore.ObjectStore

	delay    time.Duration
	gets     atomic.Int64
	inFlight atomic.Int64
	highest  atomic.Int64
}

func (s *slowGetStore) GetObject(ctx context.Context, in *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	s.gets.Add(1)
	now := s.inFlight.Add(1)
	for {
		high := s.highest.Load()
		if now <= high || s.highest.CompareAndSwap(high, now) {
			break
		}
	}
	defer s.inFlight.Add(-1)

	time.Sleep(s.delay)
	return s.ObjectStore.GetObject(ctx, in)
}

func (s *slowGetStore) peak() int64 { return s.highest.Load() }

// hangingGetStore never answers for one key, standing in for a document whose
// read runs its full retry budget before failing.
type hangingGetStore struct {
	objectstore.ObjectStore

	hangKey string
}

func (s *hangingGetStore) GetObject(ctx context.Context, in *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	if in.Key != nil && *in.Key == s.hangKey {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return s.ObjectStore.GetObject(ctx, in)
}

// TestListVolumes_BoundsOneSlowDocument covers the shape that made a single bad
// volume document a cluster-wide problem: the read did not fail quickly, so its
// latency was added to every listing that walked the prefix.
func TestListVolumes_BoundsOneSlowDocument(t *testing.T) {
	previous := listFetchTimeout
	listFetchTimeout = 50 * time.Millisecond
	t.Cleanup(func() { listFetchTimeout = previous })

	objects := objectstore.NewMemoryObjectStore()
	store := NewStore(objects, "control-plane")
	ctx := context.Background()

	require.NoError(t, store.PutVolume(ctx, Volume{VolumeID: "vol-good", TenantID: "000000000001", CapacityGiB: 1}))
	require.NoError(t, store.PutVolume(ctx, Volume{VolumeID: "vol-slow", TenantID: "000000000001", CapacityGiB: 1}))
	key, err := VolumeKey("000000000001", "vol-slow")
	require.NoError(t, err)

	hanging := NewStore(&hangingGetStore{ObjectStore: objects, hangKey: key}, "control-plane")

	start := time.Now()
	volumes, err := hanging.ListVolumes(ctx)
	elapsed := time.Since(start)

	require.NoError(t, err, "one slow document must not fail the whole listing")
	require.Len(t, volumes, 1)
	assert.Equal(t, "vol-good", volumes[0].VolumeID)
	assert.Less(t, elapsed, time.Second, "the listing must not wait on the slow document indefinitely")

	_, err = hanging.ListVolumesStrict(ctx)
	require.Error(t, err, "the strict listing must still report the document it could not read")
}

// A listing that reads one page returns a prefix of the truth and no error.
// For a strict caller that is worse than a failure, because its whole premise
// is that a document it did not see is not evidence of absence.
func TestListVolumesFollowsEveryPage(t *testing.T) {
	objects := objectstore.NewMemoryObjectStore()
	store := NewStore(objects, "control-plane")
	ctx := context.Background()

	const documents = 24
	want := make([]string, 0, documents)
	keys := make([]string, 0, documents)
	for i := range documents {
		id := fmt.Sprintf("vol-%02d", i)
		require.NoError(t, store.PutVolume(ctx, Volume{VolumeID: id, TenantID: "000000000001", CapacityGiB: 1}))
		key, err := VolumeKey("000000000001", id)
		require.NoError(t, err)
		want = append(want, id)
		keys = append(keys, key)
	}

	for _, strict := range []bool{false, true} {
		t.Run(fmt.Sprintf("strict=%v", strict), func(t *testing.T) {
			paging := &pagingStore{ObjectStore: objects, keys: keys, pageSize: 5}
			paged := NewStore(paging, "control-plane")

			list := paged.ListVolumes
			if strict {
				list = paged.ListVolumesStrict
			}
			volumes, err := list(ctx)
			require.NoError(t, err)

			got := make([]string, 0, len(volumes))
			for _, v := range volumes {
				got = append(got, v.VolumeID)
			}
			assert.Equal(t, want, got, "every page's documents must reach the caller")
			assert.Equal(t, 5, paging.requests(), "24 documents at 5 a page is 5 requests")
		})
	}
}

// A truncated page with no token cannot be followed. Returning what arrived
// would be the silent short answer, so it has to be an error for both callers.
func TestListVolumesRefusesTruncationItCannotFollow(t *testing.T) {
	objects := objectstore.NewMemoryObjectStore()
	store := NewStore(objects, "control-plane")
	ctx := context.Background()

	require.NoError(t, store.PutVolume(ctx, Volume{VolumeID: "vol-1", TenantID: "000000000001", CapacityGiB: 1}))
	key, err := VolumeKey("000000000001", "vol-1")
	require.NoError(t, err)

	broken := NewStore(&truncatedNoTokenStore{ObjectStore: objects, keys: []string{key}}, "control-plane")

	_, err = broken.ListVolumes(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "continuation token")

	_, err = broken.ListVolumesStrict(ctx)
	require.Error(t, err)
}

// pagingStore serves a prefix one page at a time the way a real S3 does, and
// counts the requests so a test can tell a followed listing from a lucky one.
type pagingStore struct {
	objectstore.ObjectStore

	keys     []string
	pageSize int

	mu    sync.Mutex
	calls int
}

func (s *pagingStore) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()

	start := 0
	if token := aws.StringValue(in.ContinuationToken); token != "" {
		n, err := strconv.Atoi(token)
		if err != nil {
			return nil, fmt.Errorf("continuation token %q was not one this store issued", token)
		}
		start = n
	}
	end := min(start+s.pageSize, len(s.keys))

	contents := make([]*s3.Object, 0, end-start)
	for _, key := range s.keys[start:end] {
		contents = append(contents, &s3.Object{Key: aws.String(key)})
	}

	out := &s3.ListObjectsV2Output{Contents: contents, IsTruncated: aws.Bool(end < len(s.keys))}
	if end < len(s.keys) {
		out.NextContinuationToken = aws.String(strconv.Itoa(end))
	}
	return out, nil
}

func (s *pagingStore) requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// truncatedNoTokenStore claims there is more to read and offers no way to read
// it, which is the one truncation a client cannot honour.
type truncatedNoTokenStore struct {
	objectstore.ObjectStore

	keys []string
}

func (s *truncatedNoTokenStore) ListObjectsV2(_ context.Context, _ *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
	contents := make([]*s3.Object, 0, len(s.keys))
	for _, key := range s.keys {
		contents = append(contents, &s3.Object{Key: aws.String(key)})
	}
	return &s3.ListObjectsV2Output{Contents: contents, IsTruncated: aws.Bool(true)}, nil
}
