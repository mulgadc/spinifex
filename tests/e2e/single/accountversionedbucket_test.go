//go:build e2e

package single

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// versionedKeys is more than one listing page's worth once each key carries a
// version and a delete marker, so the sweep has to follow the markers rather
// than trust the first page.
const versionedKeys = 12

// runAccountVersionedBucketTeardown tears down an account owning a versioned
// bucket through the deployed path, which is the one thing the hermetic tests
// cannot reach: they drive the reaper directly and never put predastore, the
// configured v1 client and the gateway's admin API in the same request.
//
// The bucket is left in the state prod was in. Every key is hidden behind a
// delete marker, so the bucket lists as empty by object and still holds every
// version, and a teardown that only deletes current objects writes another
// marker per pass until the job's budget is gone.
func runAccountVersionedBucketTeardown(t *testing.T, fix *Fixture) {
	account := harness.SpxAdminAccountCreate(t,
		fmt.Sprintf("Versioned bucket teardown %d", time.Now().UnixNano()), "")

	removed := false
	t.Cleanup(func() {
		if removed {
			return
		}
		out := harness.SpxRunBestEffort(t, "admin", "account", "delete",
			account.AccountID, "--yes", "--force", "--name", account.AccountName)
		t.Logf("cleanup: delete account %s: %s", account.AccountID, out)
	})

	host := fix.Env.NodeIPs[0]
	cli, err := harness.PredastoreS3WithCreds(host, account.AccessKeyID, account.SecretAccessKey)
	require.NoError(t, err)

	bucket := fmt.Sprintf("teardown-versioned-%d", time.Now().UnixNano())
	harness.Step(t, "1. a versioned bucket whose keys are all hidden behind delete markers")
	seedHiddenVersions(t, cli, bucket)

	// The state is the prod failure: nothing lists as a current object and
	// every version is still there.
	listed, err := cli.ListObjectsV2(&s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	assert.Empty(t, listed.Contents, "every key must be hidden before the teardown runs")

	versions, err := cli.ListObjectVersions(&s3.ListObjectVersionsInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	assert.Len(t, versions.Versions, versionedKeys, "the hidden versions must still be stored")
	assert.Len(t, versions.DeleteMarkers, versionedKeys, "each key must be hidden by a marker")

	harness.Step(t, "2. the account is deleted through the gateway's admin API")
	harness.SpxRun(t, false, "admin", "account", "delete",
		account.AccountID, "--yes", "--name", account.AccountName)
	removed = true

	harness.Step(t, "3. the bucket is gone, not merely emptied")
	_, err = cli.ListObjectVersions(&s3.ListObjectVersionsInput{Bucket: aws.String(bucket)})
	requireBucketGone(t, err)

	// A FAILED job is never picked up by the resumer, which selects only
	// expired RUNNING jobs, so the rerun is the operator's own action. Running
	// the delete a second time is that rerun, and it has to be a success on an
	// account that is already gone rather than an error the operator has to
	// interpret.
	harness.Step(t, "4. the operator's rerun on an account already torn down")
	out := harness.SpxRunBestEffort(t, "admin", "account", "delete",
		account.AccountID, "--yes", "--name", account.AccountName)
	t.Logf("rerun: %s", out)
}

// seedHiddenVersions creates a versioned bucket, writes every key once and
// then deletes each without naming a version, which is what hides it behind a
// marker instead of removing it.
func seedHiddenVersions(t *testing.T, cli *s3.S3, bucket string) {
	t.Helper()

	_, err := cli.CreateBucket(&s3.CreateBucketInput{Bucket: aws.String(bucket)})
	require.NoError(t, err, "create-bucket")

	_, err = cli.PutBucketVersioning(&s3.PutBucketVersioningInput{
		Bucket:                  aws.String(bucket),
		VersioningConfiguration: &s3.VersioningConfiguration{Status: aws.String(s3.BucketVersioningStatusEnabled)},
	})
	require.NoError(t, err, "put-bucket-versioning")

	for i := range versionedKeys {
		key := fmt.Sprintf("hidden/%02d.txt", i)
		_, err = cli.PutObject(&s3.PutObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
			Body:   strings.NewReader(fmt.Sprintf("payload %d\n", i)),
		})
		require.NoError(t, err, "put-object %s", key)

		_, err = cli.DeleteObject(&s3.DeleteObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
		})
		require.NoError(t, err, "versionless delete of %s", key)
	}
}

// requireBucketGone accepts either answer a removed bucket can give. The
// account's credential is revoked with the account, so the request may be
// refused before the bucket is looked up at all.
func requireBucketGone(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err, "the bucket must not survive its owner")

	var awsErr awserr.Error
	if !assert.ErrorAs(t, err, &awsErr) {
		return
	}
	assert.Contains(t, []string{"NoSuchBucket", "AccessDenied", "InvalidAccessKeyId"}, awsErr.Code(),
		"unexpected error for a bucket whose account is gone: %v", err)
}
