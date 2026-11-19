package objectstore

import (
	"context"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/service/s3"
)

// ownerAccountHeader asks predastore for another account's buckets. It is
// honoured only for the config service credential this store signs with, and
// the name has to match predastore's own constant — the header lives across a
// repository boundary and nothing links the two at compile time.
const ownerAccountHeader = "X-Predastore-Owner-Account-Id"

// MultipartUploadRef identifies an upload well enough to abort it. An abort
// needs all three: the upload id alone does not say where to send it.
type MultipartUploadRef struct {
	Bucket   string
	Key      string
	UploadID string
}

// ListBucketsForOwner returns the buckets belonging to accountID rather than to
// the credential making the call.
//
// This is enumeration, not access: the service credential can already open any
// bucket it can name. Without it nothing can discover what an account owns, so
// nothing can delete it either.
func (s *S3ObjectStore) ListBucketsForOwner(ctx context.Context, accountID string) ([]string, error) {
	out, err := s.client.ListBucketsWithContext(ctx, &s3.ListBucketsInput{}, withOwnerAccount(accountID))
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(out.Buckets))
	for _, bucket := range out.Buckets {
		if name := aws.StringValue(bucket.Name); name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

// ListMultipartUploads returns the uploads started against bucket and not yet
// completed. They are invisible to an object listing and hold real storage, so
// a bucket can list as empty and still have parts in it.
func (s *S3ObjectStore) ListMultipartUploads(ctx context.Context, bucket, accountID string) ([]MultipartUploadRef, error) {
	out, err := s.client.ListMultipartUploadsWithContext(ctx, &s3.ListMultipartUploadsInput{
		Bucket:              aws.String(bucket),
		ExpectedBucketOwner: expectedOwner(accountID),
	})
	if err != nil {
		return nil, err
	}

	refs := make([]MultipartUploadRef, 0, len(out.Uploads))
	for _, upload := range out.Uploads {
		if upload == nil || aws.StringValue(upload.UploadId) == "" {
			continue
		}
		refs = append(refs, MultipartUploadRef{
			Bucket:   bucket,
			Key:      aws.StringValue(upload.Key),
			UploadID: aws.StringValue(upload.UploadId),
		})
	}
	return refs, nil
}

// AbortMultipartUpload discards an upload's parts. The object it would have
// written is untouched.
func (s *S3ObjectStore) AbortMultipartUpload(ctx context.Context, upload MultipartUploadRef, accountID string) error {
	_, err := s.client.AbortMultipartUploadWithContext(ctx, &s3.AbortMultipartUploadInput{
		Bucket:              aws.String(upload.Bucket),
		Key:                 aws.String(upload.Key),
		UploadId:            aws.String(upload.UploadID),
		ExpectedBucketOwner: expectedOwner(accountID),
	})
	return err
}

// DeleteBucket removes an empty bucket.
func (s *S3ObjectStore) DeleteBucket(ctx context.Context, bucket, accountID string) error {
	_, err := s.client.DeleteBucketWithContext(ctx, &s3.DeleteBucketInput{
		Bucket:              aws.String(bucket),
		ExpectedBucketOwner: expectedOwner(accountID),
	})
	return err
}

// GetBucketVersioning reports a bucket's versioning status, empty for a bucket
// that was never versioned. That is AWS's answer too: the configuration
// carries no Status until versioning has been enabled once.
func (s *S3ObjectStore) GetBucketVersioning(ctx context.Context, bucket, accountID string) (string, error) {
	out, err := s.client.GetBucketVersioningWithContext(ctx, &s3.GetBucketVersioningInput{
		Bucket:              aws.String(bucket),
		ExpectedBucketOwner: expectedOwner(accountID),
	})
	if err != nil {
		return "", err
	}
	return aws.StringValue(out.Status), nil
}

// ListObjectVersions pages the retained versions and delete markers of a
// bucket. They are the only thing a versioned bucket's delete refuses on once
// every current key is hidden, and a versionless delete cannot remove one.
func (s *S3ObjectStore) ListObjectVersions(ctx context.Context, input *s3.ListObjectVersionsInput) (*s3.ListObjectVersionsOutput, error) {
	return s.client.ListObjectVersionsWithContext(ctx, input)
}

// expectedOwner carries the account a caller believes owns the bucket, so a
// name that changed hands since it was listed is refused rather than mutated.
// An empty account asserts nothing, which is what an unscoped caller means.
func expectedOwner(accountID string) *string {
	if accountID == "" {
		return nil
	}
	return aws.String(accountID)
}

// withOwnerAccount sets the owner header on the request before it is signed, so
// the header is covered by the signature rather than stripped as unsigned.
func withOwnerAccount(accountID string) request.Option {
	return func(r *request.Request) {
		r.HTTPRequest.Header.Set(ownerAccountHeader, accountID)
	}
}
