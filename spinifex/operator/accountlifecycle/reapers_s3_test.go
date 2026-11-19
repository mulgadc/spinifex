package accountlifecycle

//test:in-package — the reaper is unexported, and the order it empties a bucket
// in is the substance of it.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeBuckets is an in-memory object store keyed by owner, so a reaper that
// reaches past the account it was given is visible rather than merely unproven.
type fakeBuckets struct {
	mu sync.Mutex

	// owners maps account id to the buckets it owns.
	owners map[string][]string
	// bucketOwner maps bucket to the account that owns it. A bucket registered
	// here refuses any request whose expected owner is absent or different, the
	// way predastore's precondition does, so a sweep that reached mutation time
	// without the account fails here rather than on a cluster.
	bucketOwner map[string]string
	// objects maps bucket to its keys, in the order they list.
	objects map[string][]string
	// uploads maps bucket to the uploads in flight against it.
	uploads map[string][]objectstore.MultipartUploadRef
	// pageSize truncates object listings so pagination is exercised.
	pageSize int

	// versioning maps bucket to its versioning status, empty for never
	// versioned. The rules below are taken from predastore's behaviour, which
	// the whole-stack test over real HTTP is what keeps honest.
	versioning map[string]string
	// versions maps bucket to the stored versions of each key, oldest first.
	versions map[string][]fakeVersion
	// strandedRows maps bucket to listing rows whose placement is gone. They
	// are invisible to a version listing and still refuse a bucket delete, so
	// only the versionless object loop can clear them.
	strandedRows map[string][]string

	calls    []string
	deleted  []string
	failWith map[string]error
}

// fakeVersion is one stored version of a key: either a version holding bytes
// or a delete marker hiding the key behind it.
type fakeVersion struct {
	key          string
	versionID    string
	deleteMarker bool
}

func newFakeBuckets() *fakeBuckets {
	return &fakeBuckets{
		owners:       map[string][]string{},
		bucketOwner:  map[string]string{},
		objects:      map[string][]string{},
		uploads:      map[string][]objectstore.MultipartUploadRef{},
		versioning:   map[string]string{},
		versions:     map[string][]fakeVersion{},
		strandedRows: map[string][]string{},
		failWith:     map[string]error{},
	}
}

// own records who owns a bucket, which turns the owner precondition on for it.
// A bucket left unregistered asserts nothing, so a test about the sweep's order
// is not also a test about ownership.
func (f *fakeBuckets) own(accountID string, buckets ...string) {
	f.owners[accountID] = append(f.owners[accountID], buckets...)
	for _, bucket := range buckets {
		f.bucketOwner[bucket] = accountID
	}
}

// requireOwner refuses a request that does not assert the bucket's owner, the
// way predastore answers one. An absent expected owner is refused too: it
// asserts nothing, and this path deletes versions permanently.
func (f *fakeBuckets) requireOwner(bucket, expected string) error {
	owner, registered := f.bucketOwner[bucket]
	if !registered {
		return nil
	}
	if expected != owner {
		return awserr.New("AccessDenied", "Access Denied", nil)
	}
	return nil
}

// seedVersioned enables versioning and writes one version of each key, which
// is the state the prod incident was in.
func (f *fakeBuckets) seedVersioned(bucket string, keys ...string) {
	f.versioning[bucket] = "Enabled"
	for i, key := range keys {
		f.objects[bucket] = append(f.objects[bucket], key)
		f.versions[bucket] = append(f.versions[bucket], fakeVersion{
			key: key, versionID: fmt.Sprintf("v%d", i+1),
		})
	}
}

// nextVersionID names a version the way a store that never reuses an id does.
func (f *fakeBuckets) nextVersionID(bucket string) string {
	return fmt.Sprintf("m%d", len(f.versions[bucket])+1)
}

func (f *fakeBuckets) record(call string) {
	f.calls = append(f.calls, call)
}

func (f *fakeBuckets) ListBucketsForOwner(_ context.Context, accountID string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("ListBuckets")
	if err := f.failWith["ListBuckets"]; err != nil {
		return nil, err
	}
	return append([]string(nil), f.owners[accountID]...), nil
}

func (f *fakeBuckets) ListObjectsV2(_ context.Context, input *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("ListObjects")
	if err := f.failWith["ListObjects"]; err != nil {
		return nil, err
	}

	bucket := aws.StringValue(input.Bucket)
	if err := f.requireOwner(bucket, aws.StringValue(input.ExpectedBucketOwner)); err != nil {
		return nil, err
	}

	// A listing row whose placement is gone still lists as a current object,
	// and a version listing shows nothing for it.
	keys := append(append([]string(nil), f.objects[bucket]...), f.strandedRows[bucket]...)

	start := 0
	if token := aws.StringValue(input.ContinuationToken); token != "" {
		for i, key := range keys {
			if key == token {
				start = i
				break
			}
		}
	}

	end := len(keys)
	if f.pageSize > 0 && start+f.pageSize < end {
		end = start + f.pageSize
	}

	out := &s3.ListObjectsV2Output{IsTruncated: aws.Bool(end < len(keys))}
	if end < len(keys) {
		out.NextContinuationToken = aws.String(keys[end])
	}
	for _, key := range keys[start:end] {
		out.Contents = append(out.Contents, &s3.Object{Key: aws.String(key)})
	}
	return out, nil
}

func (f *fakeBuckets) DeleteObject(_ context.Context, input *s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("DeleteObject")
	if err := f.failWith["DeleteObject"]; err != nil {
		return nil, err
	}

	bucket, key := aws.StringValue(input.Bucket), aws.StringValue(input.Key)
	versionID := aws.StringValue(input.VersionId)
	if err := f.requireOwner(bucket, aws.StringValue(input.ExpectedBucketOwner)); err != nil {
		return nil, err
	}

	// A delete naming a version removes that version, and nothing else: the
	// listing key may point at a different one.
	if versionID != "" {
		f.deleted = append(f.deleted, bucket+"/"+key+"@"+versionID)
		kept := make([]fakeVersion, 0, len(f.versions[bucket]))
		for _, version := range f.versions[bucket] {
			if version.key != key || version.versionID != versionID {
				kept = append(kept, version)
			}
		}
		f.versions[bucket] = kept
		return &s3.DeleteObjectOutput{}, nil
	}

	f.deleted = append(f.deleted, bucket+"/"+key)

	// On a versioned bucket a versionless delete hides the key behind a marker
	// rather than removing anything, which is the whole prod failure: the
	// bucket then lists as empty and still refuses to delete.
	if status := f.versioning[bucket]; status == "Enabled" || status == "Suspended" {
		markerID := f.nextVersionID(bucket)
		if status == "Suspended" {
			markerID = "null"
		}
		f.versions[bucket] = append(f.versions[bucket], fakeVersion{
			key: key, versionID: markerID, deleteMarker: true,
		})
	}

	kept := make([]string, 0, len(f.objects[bucket]))
	for _, existing := range f.objects[bucket] {
		if existing != key {
			kept = append(kept, existing)
		}
	}
	f.objects[bucket] = kept

	// A versionless delete is also the only thing that clears a listing row
	// whose placement is gone, which is why the object loop is kept.
	keptStranded := make([]string, 0, len(f.strandedRows[bucket]))
	for _, existing := range f.strandedRows[bucket] {
		if existing != key {
			keptStranded = append(keptStranded, existing)
		}
	}
	f.strandedRows[bucket] = keptStranded

	return &s3.DeleteObjectOutput{}, nil
}

// ListObjectVersions reports the stored versions and markers of a bucket. A
// listing row whose placement is gone has no version, so it appears nowhere
// here while still refusing the bucket delete.
func (f *fakeBuckets) ListObjectVersions(_ context.Context, input *s3.ListObjectVersionsInput) (*s3.ListObjectVersionsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("ListVersions")
	if err := f.failWith["ListVersions"]; err != nil {
		return nil, err
	}

	bucket := aws.StringValue(input.Bucket)
	if err := f.requireOwner(bucket, aws.StringValue(input.ExpectedBucketOwner)); err != nil {
		return nil, err
	}

	stored := f.versions[bucket]

	start := 0
	if marker := aws.StringValue(input.VersionIdMarker); marker != "" {
		for i, version := range stored {
			if version.versionID == marker {
				start = i + 1
				break
			}
		}
	}

	end := len(stored)
	if f.pageSize > 0 && start+f.pageSize < end {
		end = start + f.pageSize
	}

	out := &s3.ListObjectVersionsOutput{IsTruncated: aws.Bool(end < len(stored))}
	if end < len(stored) && end > 0 {
		out.NextKeyMarker = aws.String(stored[end-1].key)
		out.NextVersionIdMarker = aws.String(stored[end-1].versionID)
	}
	for _, version := range stored[start:end] {
		if version.deleteMarker {
			out.DeleteMarkers = append(out.DeleteMarkers, &s3.DeleteMarkerEntry{
				Key: aws.String(version.key), VersionId: aws.String(version.versionID),
			})
			continue
		}
		out.Versions = append(out.Versions, &s3.ObjectVersion{
			Key: aws.String(version.key), VersionId: aws.String(version.versionID),
		})
	}
	return out, nil
}

// GetBucketVersioning answers the empty string for a bucket that was never
// versioned, which is what AWS returns as an absent Status.
func (f *fakeBuckets) GetBucketVersioning(_ context.Context, bucket, accountID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("GetVersioning")
	if err := f.failWith["GetVersioning"]; err != nil {
		return "", err
	}
	if err := f.requireOwner(bucket, accountID); err != nil {
		return "", err
	}
	return f.versioning[bucket], nil
}

func (f *fakeBuckets) ListMultipartUploads(_ context.Context, bucket, accountID string) ([]objectstore.MultipartUploadRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("ListUploads")
	if err := f.failWith["ListUploads"]; err != nil {
		return nil, err
	}
	if err := f.requireOwner(bucket, accountID); err != nil {
		return nil, err
	}
	return append([]objectstore.MultipartUploadRef(nil), f.uploads[bucket]...), nil
}

func (f *fakeBuckets) AbortMultipartUpload(_ context.Context, upload objectstore.MultipartUploadRef, accountID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("AbortUpload")
	if err := f.failWith["AbortUpload"]; err != nil {
		return err
	}
	if err := f.requireOwner(upload.Bucket, accountID); err != nil {
		return err
	}
	f.deleted = append(f.deleted, "upload:"+upload.Bucket+"/"+upload.UploadID)
	f.uploads[upload.Bucket] = nil
	return nil
}

func (f *fakeBuckets) DeleteBucket(_ context.Context, bucket, accountID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("DeleteBucket")
	if err := f.failWith["DeleteBucket"]; err != nil {
		return err
	}
	if err := f.requireOwner(bucket, accountID); err != nil {
		return err
	}
	// Every retained version and every delete marker refuses the delete, the
	// same as AWS, and so does a listing row whose placement is gone.
	if len(f.objects[bucket]) > 0 || len(f.versions[bucket]) > 0 || len(f.strandedRows[bucket]) > 0 {
		return awserr.New("BucketNotEmpty", "The bucket you tried to delete is not empty", nil)
	}
	f.deleted = append(f.deleted, "bucket:"+bucket)
	f.forget(bucket)
	return nil
}

// forget drops a deleted bucket from its owner's listing, so a second drain
// pass does not see a bucket that is gone.
func (f *fakeBuckets) forget(bucket string) {
	owner := f.bucketOwner[bucket]
	delete(f.bucketOwner, bucket)

	kept := make([]string, 0, len(f.owners[owner]))
	for _, existing := range f.owners[owner] {
		if existing != bucket {
			kept = append(kept, existing)
		}
	}
	f.owners[owner] = kept
}

var _ BucketStore = (*fakeBuckets)(nil)

func newBucketReaper(store BucketStore) *bucketReaper {
	return &bucketReaper{store: store}
}

// The platform buckets belong to the system account and are shared by every
// tenant. Deleting one takes the cluster down, so the reaper must never so
// much as return one.
func TestBucketReaperListsOnlyTheAccountsOwnBuckets(t *testing.T) {
	fake := newFakeBuckets()
	fake.owners["000000000042"] = []string{"tenant-logs", "tenant-data"}
	fake.owners["000000000000"] = []string{"predastore", "spinifex-system"}

	found, err := newBucketReaper(fake).List(testCtx(t), "000000000042")
	require.NoError(t, err)

	assert.Equal(t, []Resource{
		{Kind: "s3-bucket", ID: "tenant-logs"},
		{Kind: "s3-bucket", ID: "tenant-data"},
	}, found)
}

// Uploads are aborted before the objects go, because an abort that ran after
// the bucket delete would have nothing left to address.
func TestBucketReaperAbortsUploadsThenEmptiesThenDeletes(t *testing.T) {
	fake := newFakeBuckets()
	fake.objects["tenant-data"] = []string{"a.txt", "b.txt"}
	fake.uploads["tenant-data"] = []objectstore.MultipartUploadRef{
		{Bucket: "tenant-data", Key: "big.img", UploadID: "u-1"},
	}

	require.NoError(t, newBucketReaper(fake).Delete(testCtx(t), "000000000042",
		Resource{Kind: "s3-bucket", ID: "tenant-data"}, false))

	assert.Equal(t, []string{
		"upload:tenant-data/u-1", "tenant-data/a.txt", "tenant-data/b.txt", "bucket:tenant-data",
	}, fake.deleted)
}

// An incomplete upload is invisible to an object listing and predastore's
// bucket delete only checks for objects, so nothing else would ever notice the
// parts it holds.
func TestBucketReaperAbortsUploadsInAnOtherwiseEmptyBucket(t *testing.T) {
	fake := newFakeBuckets()
	fake.uploads["tenant-data"] = []objectstore.MultipartUploadRef{
		{Bucket: "tenant-data", Key: "big.img", UploadID: "u-1"},
	}

	require.NoError(t, newBucketReaper(fake).Delete(testCtx(t), "000000000042",
		Resource{Kind: "s3-bucket", ID: "tenant-data"}, false))

	assert.Contains(t, fake.deleted, "upload:tenant-data/u-1")
	assert.Contains(t, fake.deleted, "bucket:tenant-data")
}

// Trusting one page would leave every object past it behind, and the bucket
// delete would then fail for a reason that looks unrelated.
func TestBucketReaperFollowsObjectPagination(t *testing.T) {
	fake := newFakeBuckets()
	fake.pageSize = 2
	fake.objects["tenant-data"] = []string{"a", "b", "c", "d", "e"}

	require.NoError(t, newBucketReaper(fake).Delete(testCtx(t), "000000000042",
		Resource{Kind: "s3-bucket", ID: "tenant-data"}, false))

	assert.Empty(t, fake.objects["tenant-data"])
	assert.Contains(t, fake.deleted, "bucket:tenant-data")
}

// Teardown re-runs after a crash. An object deleted by the previous attempt
// must not stop the second one before it starts.
func TestBucketReaperTreatsAMissingObjectAsDeleted(t *testing.T) {
	fake := newFakeBuckets()
	fake.objects["tenant-data"] = []string{"a.txt"}
	fake.failWith["DeleteObject"] = &objectstore.NoSuchKeyError{Key: "a.txt"}

	err := newBucketReaper(fake).Delete(testCtx(t), "000000000042",
		Resource{Kind: "s3-bucket", ID: "tenant-data"}, false)

	// The bucket is still not empty in the fake, so the delete is refused and
	// the resource stays listed for the drain loop to retry. What matters is
	// that the missing key did not fail the sweep.
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "NoSuchKey")
}

func TestBucketReaperTreatsAMissingBucketAsDeleted(t *testing.T) {
	fake := newFakeBuckets()
	fake.failWith["DeleteBucket"] = awserr.New("NoSuchBucket", "The specified bucket does not exist", nil)

	err := newBucketReaper(fake).Delete(testCtx(t), "000000000042",
		Resource{Kind: "s3-bucket", ID: "gone"}, false)

	assert.NoError(t, err)
}

// A bucket that will not empty has to stay listed. Reporting the delete as a
// success would let the account record go with the data still in place.
func TestBucketReaperReportsABucketItCannotEmpty(t *testing.T) {
	fake := newFakeBuckets()
	fake.objects["tenant-data"] = []string{"a.txt"}
	fake.failWith["DeleteObject"] = errors.New("backend unavailable")

	err := newBucketReaper(fake).Delete(testCtx(t), "000000000042",
		Resource{Kind: "s3-bucket", ID: "tenant-data"}, false)

	require.Error(t, err)
	assert.NotContains(t, fake.deleted, "bucket:tenant-data")
}

// Buckets are storage, not compute: they must not be reaped before the
// instances and images that read from them are gone.
func TestS3ReaperRunsInTheStorageStage(t *testing.T) {
	reapers := S3Reapers(newFakeBuckets())

	require.Len(t, reapers, 1)
	assert.Equal(t, StageStorage, reapers[0].Stage())
	assert.Equal(t, "s3-bucket", reapers[0].Kind())
}

// The engine sorts reapers by stage and the sort is stable, so a bucket reaper
// registered after the EC2 storage reapers stays after them.
func TestSortReapersKeepsBucketsAfterVolumes(t *testing.T) {
	reapers := append(EC2Reapers(nil, 1, []string{"node-1"}), S3Reapers(newFakeBuckets())...)
	SortReapers(reapers)

	storage := make([]string, 0, len(reapers))
	for _, reaper := range reapers {
		if reaper.Stage() == StageStorage {
			storage = append(storage, reaper.Kind())
		}
	}

	require.NotEmpty(t, storage)
	assert.Greater(t, len(storage), 1, "the storage stage owns more than buckets")
	assert.Equal(t, "s3-bucket", storage[len(storage)-1])
}

// The prod failure, without a cluster. A versioned bucket's keys are hidden
// behind delete markers by the versionless delete that was meant to empty it,
// so the bucket lists as empty, every retained version is still there, and
// DeleteBucket answers BucketNotEmpty on every retry until the budget is gone.
func TestBucketReaperEmptiesAVersionedBucket(t *testing.T) {
	fake := newFakeBuckets()
	fake.seedVersioned("tenant-data", "a.txt", "b.txt")

	require.NoError(t, newBucketReaper(fake).Delete(testCtx(t), "000000000042",
		Resource{Kind: "s3-bucket", ID: "tenant-data"}, false))

	assert.Empty(t, fake.versions["tenant-data"], "versions or markers were left behind")
	assert.Empty(t, fake.objects["tenant-data"], "current keys were left behind")
	assert.Contains(t, fake.deleted, "bucket:tenant-data", "the bucket was not deleted")
}

// A suspended bucket still holds the versions written while it was enabled,
// and its null version is one of them.
func TestBucketReaperEmptiesASuspendedBucket(t *testing.T) {
	fake := newFakeBuckets()
	fake.seedVersioned("tenant-data", "a.txt")
	fake.versioning["tenant-data"] = "Suspended"
	fake.versions["tenant-data"] = append(fake.versions["tenant-data"],
		fakeVersion{key: "a.txt", versionID: "null"})

	require.NoError(t, newBucketReaper(fake).Delete(testCtx(t), "000000000042",
		Resource{Kind: "s3-bucket", ID: "tenant-data"}, false))

	assert.Empty(t, fake.versions["tenant-data"])
	assert.Contains(t, fake.deleted, "bucket:tenant-data")
}

// A bucket that was never versioned must not gain a version listing it has no
// answer for, and must still empty through the object loop.
func TestBucketReaperSkipsTheVersionSweepOnAnUnversionedBucket(t *testing.T) {
	fake := newFakeBuckets()
	fake.objects["tenant-data"] = []string{"a.txt", "b.txt"}

	require.NoError(t, newBucketReaper(fake).Delete(testCtx(t), "000000000042",
		Resource{Kind: "s3-bucket", ID: "tenant-data"}, false))

	assert.NotContains(t, fake.calls, "ListVersions",
		"an unversioned bucket must not be swept by version")
	assert.Equal(t, []string{
		"tenant-data/a.txt", "tenant-data/b.txt", "bucket:tenant-data",
	}, fake.deleted)
}

// The regression guarding the two-path design. A listing row whose placement
// is gone has no version to name, so a version listing shows nothing for it
// while the bucket delete still refuses on it. Only the versionless delete of
// the object loop clears it, which is why that loop is kept rather than
// replaced. This fails if the version sweep is ever made unconditional.
func TestBucketReaperClearsAListingRowWithNoPlacement(t *testing.T) {
	fake := newFakeBuckets()
	fake.strandedRows["tenant-data"] = []string{"stranded.txt"}

	require.NoError(t, newBucketReaper(fake).Delete(testCtx(t), "000000000042",
		Resource{Kind: "s3-bucket", ID: "tenant-data"}, false))

	assert.Empty(t, fake.strandedRows["tenant-data"], "the stranded listing row was left behind")
	assert.Contains(t, fake.deleted, "bucket:tenant-data")
}

// The same row inside a versioned bucket, which is the case a unified loop
// would strand: the version sweep cannot see it, so the object loop has to run
// as well.
func TestBucketReaperClearsAStrandedRowInAVersionedBucket(t *testing.T) {
	fake := newFakeBuckets()
	fake.seedVersioned("tenant-data", "a.txt")
	fake.strandedRows["tenant-data"] = []string{"stranded.txt"}

	require.NoError(t, newBucketReaper(fake).Delete(testCtx(t), "000000000042",
		Resource{Kind: "s3-bucket", ID: "tenant-data"}, false))

	assert.Empty(t, fake.versions["tenant-data"])
	assert.Empty(t, fake.strandedRows["tenant-data"])
	assert.Contains(t, fake.deleted, "bucket:tenant-data")
}

// A version listing that pages must be followed to the end, or the bucket is
// left holding everything past the first page.
func TestBucketReaperPagesTheVersionListing(t *testing.T) {
	fake := newFakeBuckets()
	fake.seedVersioned("tenant-data", "a.txt", "b.txt", "c.txt", "d.txt")
	fake.pageSize = 2

	require.NoError(t, newBucketReaper(fake).Delete(testCtx(t), "000000000042",
		Resource{Kind: "s3-bucket", ID: "tenant-data"}, false))

	assert.Empty(t, fake.versions["tenant-data"])
	assert.Contains(t, fake.deleted, "bucket:tenant-data")
}

// Ownership decided at listing time has to hold at every mutation after it,
// because a bucket name is recyclable. The fake refuses a request that does not
// assert the owner, so a sweep that dropped the account anywhere in the middle
// fails here. It does not prove the gate enforces the precondition, which is
// what predastore's own host test covers.
func TestBucketReaperAssertsTheOwnerOnEveryRequest(t *testing.T) {
	fake := newFakeBuckets()
	fake.own("000000000042", "tenant-data")
	fake.seedVersioned("tenant-data", "a.txt", "b.txt")
	fake.strandedRows["tenant-data"] = []string{"stranded.txt"}
	fake.uploads["tenant-data"] = []objectstore.MultipartUploadRef{
		{Bucket: "tenant-data", Key: "big.img", UploadID: "u-1"},
	}

	require.NoError(t, newBucketReaper(fake).Delete(testCtx(t), "000000000042",
		Resource{Kind: "s3-bucket", ID: "tenant-data"}, false))

	assert.Contains(t, fake.deleted, "bucket:tenant-data")
	// Named individually so a request that stops being issued is not silently
	// counted as one that asserted the owner.
	for _, call := range []string{
		"ListUploads", "AbortUpload", "GetVersioning", "ListObjects", "DeleteObject",
		"ListVersions", "DeleteBucket",
	} {
		assert.Contains(t, fake.calls, call, "no %s was issued, so nothing asserted the owner on it", call)
	}
}

// A name that changed hands since it was listed is refused at the first
// request, and nothing in the bucket now standing under it is touched.
func TestBucketReaperRefusedByTheOwnerPreconditionDestroysNothing(t *testing.T) {
	fake := newFakeBuckets()
	fake.own("999999999999", "tenant-data")
	fake.seedVersioned("tenant-data", "a.txt")

	err := newBucketReaper(fake).Delete(testCtx(t), "000000000042",
		Resource{Kind: "s3-bucket", ID: "tenant-data"}, false)

	require.Error(t, err)
	assert.Empty(t, fake.deleted)
	assert.Len(t, fake.versions["tenant-data"], 1, "the new owner's version was destroyed")
}

// An empty account asserts no owner at all, which the store reads as no
// precondition. Emptying a bucket under it would delete versions permanently
// with nothing checked, so the sweep refuses to start.
func TestBucketReaperRefusesToEmptyWithNoOwner(t *testing.T) {
	fake := newFakeBuckets()
	fake.seedVersioned("tenant-data", "a.txt")

	err := newBucketReaper(fake).Delete(testCtx(t), "",
		Resource{Kind: "s3-bucket", ID: "tenant-data"}, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no owner to assert")
	assert.Empty(t, fake.calls, "no request was issued")
}

// A bucket the sweep cannot empty has to keep the account. Run under the
// shortened stage budget rather than the real one, so the loop's end is the
// deadline the test set and not elapsed wall time.
//
// This pins today's behaviour and claims no improvement on it: the stall
// detector is unchanged, so a bucket that cannot empty still spends the whole
// stage budget before it is reported.
func TestBucketReaperStuckBucketKeepsTheAccountAndNamesTheCause(t *testing.T) {
	fake := newFakeBuckets()
	fake.own(testAccountID, "tenant-data")
	fake.seedVersioned("tenant-data", "a.txt")
	fake.failWith["DeleteObject"] = errors.New("backend unavailable")

	accounts := newFakeAccounts()
	engine := quietEngine(accounts, S3Reapers(fake)...)
	// The backoff between passes is counted rather than waited out, so the
	// loop ends on the budget the test set instead of on elapsed wall time.
	backoffs := 0
	engine.wait = func(ctx context.Context, _ time.Duration) error {
		backoffs++
		return ctx.Err()
	}

	result, _ := engine.Teardown(context.Background(), request())

	assert.False(t, accounts.deleted, "the account must survive a bucket that still holds data")
	assert.NotContains(t, fake.deleted, "bucket:tenant-data")

	var named bool
	for _, stage := range result.Stages {
		for _, stuck := range stage.Stuck {
			named = named || (stuck.Resource.ID == "tenant-data" &&
				strings.Contains(stuck.Reason, "backend unavailable"))
		}
	}
	assert.True(t, named, "the stuck report must name the bucket and the cause: %+v", result.Stages)
	assert.Positive(t, backoffs, "the loop must retry the bucket rather than give up on the first pass")
}
