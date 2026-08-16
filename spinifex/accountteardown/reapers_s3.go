package accountteardown

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
)

// BucketStore is the slice of the object store the bucket reaper needs.
//
// Narrow on purpose, and read from the tenant's side only: nothing here can
// create a bucket or write an object, so a teardown bug cannot leave new state
// behind in an account it is emptying.
type BucketStore interface {
	ListBucketsForOwner(ctx context.Context, accountID string) ([]string, error)
	ListObjectsV2(ctx context.Context, input *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error)
	DeleteObject(ctx context.Context, input *s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error)
	ListMultipartUploads(ctx context.Context, bucket string) ([]objectstore.MultipartUploadRef, error)
	AbortMultipartUpload(ctx context.Context, upload objectstore.MultipartUploadRef) error
	DeleteBucket(ctx context.Context, bucket string) error
}

var _ BucketStore = (*objectstore.S3ObjectStore)(nil)

// S3Reapers returns the S3-backed reapers.
//
// One reaper whose unit is a bucket, rather than separate object and bucket
// reapers. Objects are only interesting as a reason a bucket will not delete,
// and listing every object of every bucket on each sweep — which is what a
// stage's drain check does — would make emptying a large bucket quadratic.
func S3Reapers(store BucketStore) []Reaper {
	return []Reaper{&bucketReaper{store: store}}
}

// bucketPageLimit bounds a runaway pagination loop. It exists so a listing that
// never advances cannot spin the drain loop for the whole stage budget; a real
// bucket empties long before this.
const bucketPageLimit = 10000

type bucketReaper struct {
	store BucketStore
}

func (r *bucketReaper) Kind() string { return "s3-bucket" }
func (r *bucketReaper) Stage() Stage { return StageStorage }

// List returns only the buckets the account owns. The platform buckets — the
// config bucket and the system bucket holding volume metadata, AMIs and key
// material — belong to the system account and are shared by every tenant, so
// the owner scoping is the whole safety property rather than a filter applied
// afterwards.
func (r *bucketReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	names, err := r.store.ListBucketsForOwner(ctx, accountID)
	if err != nil {
		return nil, err
	}

	found := make([]Resource, 0, len(names))
	for _, name := range names {
		found = append(found, Resource{Kind: r.Kind(), ID: name})
	}
	return found, nil
}

// Delete empties the bucket and then removes it. All three steps run on every
// call: a bucket that could not be deleted last time is still listed, and the
// retry has to re-check what is in it rather than assume the first pass emptied
// it.
func (r *bucketReaper) Delete(ctx context.Context, _ string, resource Resource, _ bool) error {
	bucket := resource.ID

	if err := r.abortUploads(ctx, bucket); err != nil {
		return err
	}
	if err := r.deleteObjects(ctx, bucket); err != nil {
		return err
	}

	return ignoreAlreadyGone(r.store.DeleteBucket(ctx, bucket))
}

// abortUploads discards uploads in flight. They are invisible to an object
// listing and hold real storage, so a bucket can list as empty and still have
// parts in it — and predastore's bucket delete only checks for objects, so
// nothing else would ever notice them.
func (r *bucketReaper) abortUploads(ctx context.Context, bucket string) error {
	uploads, err := r.store.ListMultipartUploads(ctx, bucket)
	if err != nil {
		return ignoreAlreadyGone(err)
	}

	for _, upload := range uploads {
		if err := r.store.AbortMultipartUpload(ctx, upload); err != nil && !isAlreadyGone(err) {
			return fmt.Errorf("abort upload %s in %s: %w", upload.UploadID, bucket, err)
		}
	}
	return nil
}

// deleteObjects empties the bucket a page at a time, re-listing rather than
// trusting one page. Objects go one at a time because predastore serves no
// batch delete.
func (r *bucketReaper) deleteObjects(ctx context.Context, bucket string) error {
	var token *string

	for range bucketPageLimit {
		out, err := r.store.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(bucket),
			ContinuationToken: token,
		})
		if err != nil {
			return ignoreAlreadyGone(err)
		}

		for _, object := range out.Contents {
			if object == nil || aws.StringValue(object.Key) == "" {
				continue
			}
			_, err := r.store.DeleteObject(ctx, &s3.DeleteObjectInput{
				Bucket: aws.String(bucket),
				Key:    object.Key,
			})
			// A key that is already gone is a success: teardown re-runs after a
			// crash and would otherwise never get past the first object.
			if err != nil && !objectstore.IsNoSuchKeyError(err) && !isAlreadyGone(err) {
				return fmt.Errorf("delete %s/%s: %w", bucket, aws.StringValue(object.Key), err)
			}
		}

		if !aws.BoolValue(out.IsTruncated) || aws.StringValue(out.NextContinuationToken) == "" {
			return nil
		}
		token = out.NextContinuationToken
	}
	return fmt.Errorf("bucket %s still listing objects after %d pages", bucket, bucketPageLimit)
}
