//go:build e2e

package harness

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/stretchr/testify/require"
)

// ECRRegistrySuffix is the internal DNS suffix the registry host is built from.
// Defaults to the gateway's DefaultAWSInternalSuffix; override per cluster with
// SPINIFEX_ECR_SUFFIX.
func ECRRegistrySuffix() string { return getenv("SPINIFEX_ECR_SUFFIX", "spinifex.internal") }

// ECRRegistryRegion is the region label in the registry host. Mirrors the AWS
// client region.
func ECRRegistryRegion() string { return getenv("SPINIFEX_AWS_REGION", "ap-southeast-2") }

// ECRRegistryPort is the awsgw port appended to the registry host for OCI
// clients. Empty by default (production parity, :443); set SPINIFEX_ECR_PORT on
// dev clusters where awsgw binds a non-standard port (e.g. 9999).
func ECRRegistryPort() string { return getenv("SPINIFEX_ECR_PORT", "") }

// ECRRegistryHost returns {account}.dkr.ecr.{region}.{suffix}[:port] — the OCI
// registry endpoint docker/crane/skopeo address. The bare host must resolve to
// the awsgw bind IP (DNS or /etc/hosts) for the data-plane subtests to run.
func ECRRegistryHost(account string) string {
	host := account + ".dkr.ecr." + ECRRegistryRegion() + "." + ECRRegistrySuffix()
	if p := ECRRegistryPort(); p != "" {
		host += ":" + p
	}
	return host
}

// ECRRepositoryURI returns the full push/pull reference for repo under account.
func ECRRepositoryURI(account, repo string) string {
	return ECRRegistryHost(account) + "/" + repo
}

// CreateECRRepository creates repo (idempotent: an existing repo is treated as
// success) and registers best-effort teardown.
func CreateECRRepository(t *testing.T, c *AWSClient, repo string) {
	t.Helper()
	_, err := c.ECR.CreateRepository(&ecr.CreateRepositoryInput{
		RepositoryName: aws.String(repo),
	})
	if err != nil && !strings.Contains(err.Error(), "RepositoryAlreadyExistsException") {
		require.NoError(t, err)
	}
	t.Cleanup(func() { DeleteECRRepositoryBestEffort(c, repo) })
}

// DeleteECRRepositoryBestEffort force-deletes repo and all its images, swallowing
// errors — used for cleanup.
func DeleteECRRepositoryBestEffort(c *AWSClient, repo string) {
	_, _ = c.ECR.DeleteRepository(&ecr.DeleteRepositoryInput{
		RepositoryName: aws.String(repo),
		Force:          aws.Bool(true),
	})
}

// ECRGetLoginPassword returns the decoded registry password (the JWT half of the
// authorization token) for docker login -u AWS --password-stdin.
func ECRGetLoginPassword(t *testing.T, c *AWSClient) string {
	t.Helper()
	out, err := c.ECR.GetAuthorizationToken(&ecr.GetAuthorizationTokenInput{})
	require.NoError(t, err)
	require.NotEmpty(t, out.AuthorizationData, "no authorization data returned")
	raw, err := base64.StdEncoding.DecodeString(aws.StringValue(out.AuthorizationData[0].AuthorizationToken))
	require.NoError(t, err)
	user, pass, found := strings.Cut(string(raw), ":")
	require.True(t, found && user == "AWS", "unexpected authorization token shape")
	return pass
}

// ECRDescribeImageTags returns the set of image tags currently in repo.
func ECRDescribeImageTags(t *testing.T, c *AWSClient, repo string) []string {
	t.Helper()
	out, err := c.ECR.DescribeImages(&ecr.DescribeImagesInput{
		RepositoryName: aws.String(repo),
	})
	require.NoError(t, err)
	var tags []string
	for _, img := range out.ImageDetails {
		for _, tag := range img.ImageTags {
			tags = append(tags, aws.StringValue(tag))
		}
	}
	return tags
}

// ECRWaitImageTag polls DescribeImages until tag appears in repo or the deadline
// passes.
func ECRWaitImageTag(t *testing.T, c *AWSClient, repo, tag string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, got := range ECRDescribeImageTags(t, c, repo) {
			if got == tag {
				return true
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}
