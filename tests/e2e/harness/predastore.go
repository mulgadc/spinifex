//go:build e2e

package harness

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
)

// lifecycleBucketPrefix names the per-run bucket the contract test owns. A
// dedicated bucket the harness admin identity owns sidesteps the config-defined
// system bucket, which belongs to a different account and rejects cross-account
// writes.
const lifecycleBucketPrefix = "predastore-lifecycle-"

// AssertPredastoreObjectLifecycle verifies predastore's user-visible object
// contract end to end against the deployed S3 endpoint on host: an object is
// readable and intact after PutObject, absent after DeleteObject (both GET and
// ListObjectsV2 stop returning it), and the store keeps serving fresh writes
// once the delete has landed.
func AssertPredastoreObjectLifecycle(ctx context.Context, t *testing.T, host string) {
	t.Helper()
	Phase(t, "Predastore — Object Lifecycle Contract")

	cli, err := newPredastoreS3(host)
	if err != nil {
		t.Fatalf("predastore: s3 client: %v", err)
	}

	bucket := fmt.Sprintf("%s%d", lifecycleBucketPrefix, time.Now().UnixNano())
	if _, err := cli.CreateBucketWithContext(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("predastore: create bucket %s: %v", bucket, err)
	}
	t.Cleanup(func() {
		if _, err := cli.DeleteBucket(&s3.DeleteBucketInput{Bucket: aws.String(bucket)}); err != nil {
			t.Logf("predastore: cleanup delete bucket %s: %v", bucket, err)
		}
	})

	const key = "lifecycle/object"
	payload := bytes.Repeat([]byte("p"), 256<<10) // 256 KiB

	// Write, then confirm the object is readable and byte-identical.
	if _, err := cli.PutObjectWithContext(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(payload),
	}); err != nil {
		t.Fatalf("predastore: put %s: %v", key, err)
	}
	got, err := getObjectBytes(ctx, cli, bucket, key)
	if err != nil {
		t.Fatalf("predastore: get %s after put: %v", key, err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("predastore: get %s returned %d bytes, want %d", key, len(got), len(payload))
	}

	// Delete, then confirm the object is gone from both GET and List.
	if _, err := cli.DeleteObjectWithContext(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}); err != nil {
		t.Fatalf("predastore: delete %s: %v", key, err)
	}
	if _, err := cli.GetObjectWithContext(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}); !isNoSuchKey(err) {
		t.Fatalf("predastore: get %s after delete: want NoSuchKey, got %v", key, err)
	}
	list, err := cli.ListObjectsV2WithContext(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(key),
	})
	if err != nil {
		t.Fatalf("predastore: list after delete: %v", err)
	}
	if n := len(list.Contents); n != 0 {
		t.Fatalf("predastore: list after delete returned %d objects, want 0", n)
	}

	// The store must keep serving once the delete has landed: a fresh write
	// round-trips, proving the delete did not wedge the bucket.
	const key2 = "lifecycle/after-delete"
	t.Cleanup(func() {
		_, _ = cli.DeleteObject(&s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key2)})
	})
	if _, err := cli.PutObjectWithContext(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key2),
		Body:   bytes.NewReader(payload),
	}); err != nil {
		t.Fatalf("predastore: put %s after delete: %v", key2, err)
	}
	if _, err := getObjectBytes(ctx, cli, bucket, key2); err != nil {
		t.Fatalf("predastore: get %s after delete: %v", key2, err)
	}

	Detail(t, "bucket", bucket, "objectBytes", len(payload))
}

// getObjectBytes fetches an object and returns its full body.
func getObjectBytes(ctx context.Context, cli *s3.S3, bucket, key string) ([]byte, error) {
	out, err := cli.GetObjectWithContext(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = out.Body.Close() }()
	return io.ReadAll(out.Body)
}

// isNoSuchKey reports whether err is the S3 NoSuchKey error predastore returns
// for a GET on a deleted or missing key.
func isNoSuchKey(err error) bool {
	var aerr awserr.Error
	if errors.As(err, &aerr) {
		return aerr.Code() == s3.ErrCodeNoSuchKey
	}
	return false
}

// newPredastoreS3 builds an S3 client pointed at a predastore endpoint on host.
// The gateway (:9999) does not proxy S3 object operations, so the client targets
// predastore directly on predastoreHealthPort. Credentials resolve from
// SPINIFEX_AWS_* or the spinifex profile — the admin identity
// (AdministratorAccess), which owns and is authorized for the bucket the test
// creates. TLS verification is skipped (test-only) since the assertion carries
// no Env for CA load.
func newPredastoreS3(host string) (*s3.S3, error) {
	if host == "" {
		return nil, errors.New("predastore: empty host")
	}
	endpoint := fmt.Sprintf("https://%s:%d", host, predastoreHealthPort)
	cfg := &aws.Config{
		Endpoint:         aws.String(endpoint),
		Region:           aws.String(getenv("SPINIFEX_AWS_REGION", "ap-southeast-2")),
		S3ForcePathStyle: aws.Bool(true),
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test harness
			},
		},
	}
	opts := session.Options{Config: *cfg}
	if id, secret := os.Getenv("SPINIFEX_AWS_ACCESS_KEY_ID"), os.Getenv("SPINIFEX_AWS_SECRET_ACCESS_KEY"); id != "" && secret != "" {
		cfg.Credentials = credentials.NewStaticCredentials(id, secret, "")
		opts.Config = *cfg
	} else {
		opts.SharedConfigState = session.SharedConfigEnable
		opts.Profile = getenv("AWS_PROFILE", "spinifex")
	}
	sess, err := session.NewSessionWithOptions(opts)
	if err != nil {
		return nil, fmt.Errorf("predastore: s3 session: %w", err)
	}
	return s3.New(sess), nil
}
