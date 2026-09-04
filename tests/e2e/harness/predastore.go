//go:build e2e

package harness

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
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

// servingBucketPrefix names the per-check bucket AssertPredastoreServing owns.
const servingBucketPrefix = "predastore-serving-"

// servingHoldRounds is how many consecutive round trips have to succeed before
// the backend is called restored, and servingHoldGap is the pause between them.
//
// One is not enough. A gate can answer the round trip that arrives just after a
// thaw and then lose its blob peers under the load that follows, which is a
// backend that never recovered reported as one that did. Three rounds over
// ~20s is long enough to cross that window and short enough to stay in budget.
const (
	servingHoldRounds = 3
	servingHoldGap    = 10 * time.Second
)

// AssertPredastoreServing waits until every host round-trips an object, holds
// that result across servingHoldRounds, and fails the test if it never does
// within budget.
//
// Restoring the process a fault stopped is not the same as restoring the
// service. A backend can answer systemctl, hold its listeners and still serve
// nothing, so a fault injector that stops at the process leaves whatever runs
// next to discover the difference.
//
// The round trip writes as well as reads, deliberately. Under RS(2,1) a read
// reconstructs from a single surviving shard while a write needs two durable,
// so a read-only probe passes on a cluster that cannot serve a guest at all.
func AssertPredastoreServing(ctx context.Context, t *testing.T, hosts []string, budget time.Duration) {
	t.Helper()
	if len(hosts) == 0 {
		return
	}

	bucket := fmt.Sprintf("%s%d", servingBucketPrefix, time.Now().UnixNano())
	payload := bytes.Repeat([]byte("s"), 64<<10)
	deadline := time.Now().Add(budget)

	var lastErr error
	var held int
	for {
		lastErr = predastoreRoundTrip(ctx, hosts, bucket, payload)
		switch {
		case lastErr == nil:
			held++
			if held >= servingHoldRounds {
				Detail(t, "backendServingOn", strings.Join(hosts, ","))
				Detail(t, "backendServingHeldRounds", strconv.Itoa(held))
				AssertPredastoreReady(ctx, t, hosts)
				return
			}
		case held > 0:
			// A gate that served and then stopped is the failure this hold is
			// here to catch, so the count starts again rather than decaying.
			t.Logf("predastore served %d round(s) then failed, restarting the hold: %v", held, lastErr)
			held = 0
		}

		if time.Now().After(deadline) || ctx.Err() != nil {
			t.Errorf("predastore did not hold %d serving round trips through %v within %s (last error: %v)",
				servingHoldRounds, hosts, budget, lastErr)
			return
		}
		time.Sleep(servingHoldGap)
	}
}

// predastoreAdminPort serves /healthz and /readyz on each host's cluster plane.
const predastoreAdminPort = 8660

// AssertPredastoreReady checks each host's readiness probe and reports any
// failed check by name.
//
// The round trip above proves this workstation can write through a gate. It
// says nothing about which blob peers that gate can reach, and a gate one node
// short of the write floor serves a small object today and fails a guest
// tomorrow. /readyz is where that is answerable, so a host that serves it and
// reports a failed check fails here; one that does not serve it is reported and
// skipped, since the admin listener is optional configuration.
func AssertPredastoreReady(ctx context.Context, t *testing.T, hosts []string) {
	t.Helper()
	for _, host := range hosts {
		failed, err := predastoreReadyFailures(ctx, predastoreReadyURL(host))
		if err != nil {
			t.Logf("predastore readiness on %s is unavailable, skipping: %v", host, err)
			continue
		}
		if len(failed) > 0 {
			t.Errorf("predastore on %s reports failed readiness checks after the fault was cleared: %v",
				host, failed)
			continue
		}
		Detail(t, "backendReady", host)
	}
}

// predastoreReadyURL is the readiness probe on a host's cluster plane.
func predastoreReadyURL(host string) string {
	return fmt.Sprintf("http://%s:%d/readyz", host, predastoreAdminPort)
}

// predastoreReadyFailures names the checks /readyz reports as failed. An
// unready process is not itself an error here -- the failed check names are the
// finding, and an empty list from a 503 would be the probe hiding one.
func predastoreReadyFailures(ctx context.Context, url string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var body struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode %s: %w", url, err)
	}
	if len(body.Checks) == 0 && body.Status != "ready" {
		return nil, fmt.Errorf("%s reported %q with no checks", url, body.Status)
	}

	failed := make([]string, 0, len(body.Checks))
	for name, state := range body.Checks {
		if state != "ok" {
			failed = append(failed, name+"="+state)
		}
	}
	sort.Strings(failed)
	return failed, nil
}

// predastoreRoundTrip puts and gets one object through every host, so a single
// unusable node fails the check whichever gate it is reached through.
func predastoreRoundTrip(ctx context.Context, hosts []string, bucket string, payload []byte) error {
	first, err := newPredastoreS3(hosts[0])
	if err != nil {
		return err
	}
	if _, err := first.CreateBucketWithContext(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(bucket),
	}); err != nil && !isBucketExists(err) {
		return fmt.Errorf("create bucket %s on %s: %w", bucket, hosts[0], err)
	}

	for _, host := range hosts {
		cli, err := newPredastoreS3(host)
		if err != nil {
			return err
		}
		key := "serving/" + host
		if _, err := cli.PutObjectWithContext(ctx, &s3.PutObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
			Body:   bytes.NewReader(payload),
		}); err != nil {
			return fmt.Errorf("put through %s: %w", host, err)
		}
		got, err := getObjectBytes(ctx, cli, bucket, key)
		if err != nil {
			return fmt.Errorf("get through %s: %w", host, err)
		}
		if !bytes.Equal(got, payload) {
			return fmt.Errorf("get through %s returned %d bytes, want %d", host, len(got), len(payload))
		}
		if _, err := cli.DeleteObjectWithContext(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
		}); err != nil {
			return fmt.Errorf("delete through %s: %w", host, err)
		}
	}

	if _, err := first.DeleteBucketWithContext(ctx, &s3.DeleteBucketInput{
		Bucket: aws.String(bucket),
	}); err != nil {
		return fmt.Errorf("delete bucket %s: %w", bucket, err)
	}
	return nil
}

// isBucketExists reports whether err is the S3 error for a bucket the caller
// already owns, which a retried round trip is expected to hit.
func isBucketExists(err error) bool {
	var aerr awserr.Error
	if errors.As(err, &aerr) {
		return aerr.Code() == s3.ErrCodeBucketAlreadyExists ||
			aerr.Code() == s3.ErrCodeBucketAlreadyOwnedByYou
	}
	return false
}
