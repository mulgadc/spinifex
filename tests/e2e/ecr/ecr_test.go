//go:build e2e

package ecr

import (
	"fmt"
	"time"

	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// uniqueRepo returns a collision-resistant repo name scoped to a test.
func uniqueRepo(prefix string) string {
	return fmt.Sprintf("e2e/%s-%d", prefix, time.Now().UnixNano())
}

// TestECRControlPlane round-trips the ECR control-plane surface: repository
// CRUD, repository policy, and lifecycle policy. Zero VMs, pure SDK.
func TestECRControlPlane(t *testing.T) {
	f := requireECRFixture(t)
	c := f.AWS
	repo := uniqueRepo("ctl")
	harness.CreateECRRepository(t, c, repo)

	t.Run("DescribeRepositories lists the new repo", func(t *testing.T) {
		out, err := c.ECR.DescribeRepositories(&ecr.DescribeRepositoriesInput{
			RepositoryNames: []*string{aws.String(repo)},
		})
		require.NoError(t, err)
		require.Len(t, out.Repositories, 1)
		assert.Equal(t, repo, aws.StringValue(out.Repositories[0].RepositoryName))
		assert.Contains(t, aws.StringValue(out.Repositories[0].RepositoryUri), repo)
	})

	t.Run("repository policy round-trips", func(t *testing.T) {
		policy := `{"Version":"2012-10-17","Statement":[{"Sid":"allow-pull","Effect":"Allow","Principal":"*","Action":["ecr:GetDownloadUrlForLayer","ecr:BatchGetImage"]}]}`
		_, err := c.ECR.SetRepositoryPolicy(&ecr.SetRepositoryPolicyInput{
			RepositoryName: aws.String(repo),
			PolicyText:     aws.String(policy),
		})
		require.NoError(t, err)

		got, err := c.ECR.GetRepositoryPolicy(&ecr.GetRepositoryPolicyInput{
			RepositoryName: aws.String(repo),
		})
		require.NoError(t, err)
		assert.JSONEq(t, policy, aws.StringValue(got.PolicyText))
	})

	t.Run("lifecycle policy round-trips", func(t *testing.T) {
		policy := `{"rules":[{"rulePriority":1,"selection":{"tagStatus":"untagged","countType":"sinceImagePushed","countUnit":"days","countNumber":14},"action":{"type":"expire"}}]}`
		_, err := c.ECR.PutLifecyclePolicy(&ecr.PutLifecyclePolicyInput{
			RepositoryName:      aws.String(repo),
			LifecyclePolicyText: aws.String(policy),
		})
		require.NoError(t, err)

		got, err := c.ECR.GetLifecyclePolicy(&ecr.GetLifecyclePolicyInput{
			RepositoryName: aws.String(repo),
		})
		require.NoError(t, err)
		assert.JSONEq(t, policy, aws.StringValue(got.LifecyclePolicyText))
	})

	t.Run("DeleteRepository removes it", func(t *testing.T) {
		_, err := c.ECR.DeleteRepository(&ecr.DeleteRepositoryInput{
			RepositoryName: aws.String(repo),
			Force:          aws.Bool(true),
		})
		require.NoError(t, err)

		_, err = c.ECR.DescribeRepositories(&ecr.DescribeRepositoriesInput{
			RepositoryNames: []*string{aws.String(repo)},
		})
		require.Error(t, err, "deleted repo must not be describable")
		assert.Contains(t, err.Error(), "RepositoryNotFoundException")
	})
}

// TestECRGetLoginPassword confirms GetAuthorizationToken returns a usable
// AWS:<jwt> credential — the input to docker login.
func TestECRGetLoginPassword(t *testing.T) {
	f := requireECRFixture(t)
	pass := harness.ECRGetLoginPassword(t, f.AWS)
	assert.NotEmpty(t, pass)
	// JWT shape: three dot-separated segments.
	assert.Len(t, splitDots(pass), 3, "authorization password should be a JWT")
}

func splitDots(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}
