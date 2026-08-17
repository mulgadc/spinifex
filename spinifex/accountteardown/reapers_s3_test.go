package accountteardown

//test:in-package — the reaper is unexported, and the order it empties a bucket
// in is the substance of it.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeBuckets is an in-memory object store keyed by owner, so a reaper that
// reaches past the account it was given is visible rather than merely unproven.
type fakeBuckets struct {
	mu sync.Mutex

	// owners maps account id to the buckets it owns.
	owners map[string][]string
	// objects maps bucket to its keys, in the order they list.
	objects map[string][]string
	// uploads maps bucket to the uploads in flight against it.
	uploads map[string][]objectstore.MultipartUploadRef
	// pageSize truncates object listings so pagination is exercised.
	pageSize int

	calls    []string
	deleted  []string
	failWith map[string]error
}

func newFakeBuckets() *fakeBuckets {
	return &fakeBuckets{
		owners:   map[string][]string{},
		objects:  map[string][]string{},
		uploads:  map[string][]objectstore.MultipartUploadRef{},
		failWith: map[string]error{},
	}
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
	keys := f.objects[bucket]

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
	f.deleted = append(f.deleted, bucket+"/"+key)

	kept := make([]string, 0, len(f.objects[bucket]))
	for _, existing := range f.objects[bucket] {
		if existing != key {
			kept = append(kept, existing)
		}
	}
	f.objects[bucket] = kept
	return &s3.DeleteObjectOutput{}, nil
}

func (f *fakeBuckets) ListMultipartUploads(_ context.Context, bucket string) ([]objectstore.MultipartUploadRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("ListUploads")
	if err := f.failWith["ListUploads"]; err != nil {
		return nil, err
	}
	return append([]objectstore.MultipartUploadRef(nil), f.uploads[bucket]...), nil
}

func (f *fakeBuckets) AbortMultipartUpload(_ context.Context, upload objectstore.MultipartUploadRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("AbortUpload")
	if err := f.failWith["AbortUpload"]; err != nil {
		return err
	}
	f.deleted = append(f.deleted, "upload:"+upload.Bucket+"/"+upload.UploadID)
	f.uploads[upload.Bucket] = nil
	return nil
}

func (f *fakeBuckets) DeleteBucket(_ context.Context, bucket string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("DeleteBucket")
	if err := f.failWith["DeleteBucket"]; err != nil {
		return err
	}
	if len(f.objects[bucket]) > 0 {
		return awserr.New("BucketNotEmpty", "The bucket you tried to delete is not empty", nil)
	}
	f.deleted = append(f.deleted, "bucket:"+bucket)
	delete(f.owners, bucket)
	return nil
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
	reapers := append(EC2Reapers(nil, 1), S3Reapers(newFakeBuckets())...)
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
