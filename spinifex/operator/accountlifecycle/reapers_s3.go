package accountlifecycle

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
)

// BucketStore is the slice of the object store the bucket reaper needs.
//
// Narrow on purpose, and read from the tenant's side only: nothing here can
// create a bucket or write an object, so a teardown bug cannot leave new state
// behind in an account it is emptying.
// Every call takes the account the bucket was listed under, which the store
// sends as the expected bucket owner. A bucket name is recyclable, so a name
// that changed hands after the listing must be refused rather than emptied.
type BucketStore interface {
	ListBucketsForOwner(ctx context.Context, accountID string) ([]string, error)
	GetBucketVersioning(ctx context.Context, bucket, accountID string) (string, error)
	ListObjectsV2(ctx context.Context, input *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error)
	ListObjectVersions(ctx context.Context, input *s3.ListObjectVersionsInput) (*s3.ListObjectVersionsOutput, error)
	DeleteObject(ctx context.Context, input *s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error)
	ListMultipartUploads(ctx context.Context, bucket, accountID string) ([]objectstore.MultipartUploadRef, error)
	AbortMultipartUpload(ctx context.Context, upload objectstore.MultipartUploadRef, accountID string) error
	DeleteBucket(ctx context.Context, bucket, accountID string) error
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
// The account is carried to every call rather than used only to list, because
// ownership decided at listing time has to hold at each mutation after it.
func (r *bucketReaper) Delete(ctx context.Context, accountID string, resource Resource, _ bool) error {
	bucket := resource.ID

	// An empty account asserts no owner, and the store reads that as no
	// precondition at all. Refusing is the only safe reading of it here: this
	// path deletes versions permanently.
	if accountID == "" {
		return fmt.Errorf("refusing to empty %s with no owner to assert", bucket)
	}

	if err := r.abortUploads(ctx, bucket, accountID); err != nil {
		return err
	}
	if err := r.deleteObjects(ctx, bucket, accountID); err != nil {
		return err
	}

	return ignoreAlreadyGone(r.store.DeleteBucket(ctx, bucket, accountID))
}

// abortUploads discards uploads in flight. They are invisible to an object
// listing and hold real storage, so a bucket can list as empty and still have
// parts in it — and predastore's bucket delete only checks for objects, so
// nothing else would ever notice them.
func (r *bucketReaper) abortUploads(ctx context.Context, bucket, accountID string) error {
	uploads, err := r.store.ListMultipartUploads(ctx, bucket, accountID)
	if err != nil {
		return ignoreAlreadyGone(err)
	}

	for _, upload := range uploads {
		if err := r.store.AbortMultipartUpload(ctx, upload, accountID); err != nil && !isAlreadyGone(err) {
			return fmt.Errorf("abort upload %s in %s: %w", upload.UploadID, bucket, err)
		}
	}
	return nil
}

// deleteObjects empties the bucket by whichever path can empty it, which the
// versioning status decides. The two are not interchangeable.
//
// The version sweep is the only way to remove a retained version or a delete
// marker, and on a versioned bucket a versionless delete adds a marker instead
// of removing anything. Removing a key's last version also drops its listing
// row, so the sweep alone empties a versioned bucket.
//
// The object loop is the only way to clear a listing row whose placement is
// gone. It has no version to name, a version listing shows nothing for it, and
// the bucket delete still refuses on it.
func (r *bucketReaper) deleteObjects(ctx context.Context, bucket, accountID string) error {
	status, err := r.store.GetBucketVersioning(ctx, bucket, accountID)
	if err != nil {
		return ignoreAlreadyGone(err)
	}

	if status == s3.BucketVersioningStatusEnabled || status == s3.BucketVersioningStatusSuspended {
		return r.deleteVersions(ctx, bucket, accountID)
	}
	return r.deleteCurrentObjects(ctx, bucket, accountID)
}

// deleteVersions removes every retained version and delete marker a page at a
// time, re-listing rather than trusting one page.
//
// bucketPageLimit bounds this loop's iterations and nothing inside a
// predastore request: a version listing reads the bucket-wide key index per
// page and returns a single key's whole history at once, so a key with a long
// enough history comes back in one page however small the limit.
func (r *bucketReaper) deleteVersions(ctx context.Context, bucket, accountID string) error {
	var keyMarker, versionMarker *string

	for range bucketPageLimit {
		out, err := r.store.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{
			Bucket:              aws.String(bucket),
			KeyMarker:           keyMarker,
			VersionIdMarker:     versionMarker,
			ExpectedBucketOwner: aws.String(accountID),
		})
		if err != nil {
			return ignoreAlreadyGone(err)
		}

		for _, version := range out.Versions {
			if version == nil {
				continue
			}
			if err := r.deleteVersion(ctx, bucket, accountID,
				aws.StringValue(version.Key), aws.StringValue(version.VersionId)); err != nil {
				return err
			}
		}
		// A delete marker is a version: AWS refuses the bucket delete while one
		// remains, and removing it is what unhides the key underneath.
		for _, marker := range out.DeleteMarkers {
			if marker == nil {
				continue
			}
			if err := r.deleteVersion(ctx, bucket, accountID,
				aws.StringValue(marker.Key), aws.StringValue(marker.VersionId)); err != nil {
				return err
			}
		}

		if !aws.BoolValue(out.IsTruncated) || aws.StringValue(out.NextKeyMarker) == "" {
			return nil
		}
		keyMarker, versionMarker = out.NextKeyMarker, out.NextVersionIdMarker
	}
	return fmt.Errorf("bucket %s still listing versions after %d pages", bucket, bucketPageLimit)
}

// deleteVersion removes one named version. A version that is already gone is a
// success, the same as a key: teardown re-runs after a crash.
func (r *bucketReaper) deleteVersion(ctx context.Context, bucket, accountID, key, versionID string) error {
	if key == "" || versionID == "" {
		return nil
	}
	_, err := r.store.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket:              aws.String(bucket),
		Key:                 aws.String(key),
		VersionId:           aws.String(versionID),
		ExpectedBucketOwner: aws.String(accountID),
	})
	if err != nil && !objectstore.IsNoSuchKeyError(err) && !isAlreadyGone(err) {
		return fmt.Errorf("delete %s/%s version %s: %w", bucket, key, versionID, err)
	}
	return nil
}

// deleteCurrentObjects empties the bucket a page at a time, re-listing rather
// than trusting one page. Objects go one at a time because predastore serves no
// batch delete.
func (r *bucketReaper) deleteCurrentObjects(ctx context.Context, bucket, accountID string) error {
	var token *string

	for range bucketPageLimit {
		out, err := r.store.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:              aws.String(bucket),
			ContinuationToken:   token,
			ExpectedBucketOwner: aws.String(accountID),
		})
		if err != nil {
			return ignoreAlreadyGone(err)
		}

		for _, object := range out.Contents {
			if object == nil || aws.StringValue(object.Key) == "" {
				continue
			}
			_, err := r.store.DeleteObject(ctx, &s3.DeleteObjectInput{
				Bucket:              aws.String(bucket),
				Key:                 object.Key,
				ExpectedBucketOwner: aws.String(accountID),
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
