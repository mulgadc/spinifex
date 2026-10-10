package awsapi

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/service/ecr"
	ecrdomain "github.com/mulgadc/spinifex/spinifex/domains/ecr"
	ecrregistry "github.com/mulgadc/spinifex/spinifex/domains/ecr/registry"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const lifecyclePreviewPolicy = `{"rules":[{"rulePriority":7,"selection":{"tagStatus":"any","countType":"imageCountMoreThan","countNumber":1},"action":{"type":"expire"}}]}`

type fakeLifecyclePolicyStore struct {
	policy []byte
	err    error
}

func (s fakeLifecyclePolicyStore) GetLifecyclePolicy(context.Context, string, string) ([]byte, error) {
	return s.policy, s.err
}

func TestStartLifecyclePolicyPreview_EvaluatesOverride(t *testing.T) {
	catalog := fakeImageCatalog{records: []ecrregistry.ImageRecord{
		{Digest: "sha256:older", Tags: []string{"v1"}, PushedAt: time.Now().Add(-time.Hour)},
		{Digest: "sha256:newer", Tags: []string{"v2"}, PushedAt: time.Now()},
	}}
	encodedPolicy, err := json.Marshal(lifecyclePreviewPolicy)
	require.NoError(t, err)
	body := []byte(`{"repositoryName":"team/app","lifecyclePolicyText":` + string(encodedPolicy) + `}`)

	out, err := StartLifecyclePolicyPreview(context.Background(), fakeLifecyclePolicyStore{}, catalog, "123456789012", body)
	require.NoError(t, err)
	assert.Equal(t, "team/app", *out.RepositoryName)
	assert.Equal(t, lifecyclePreviewPolicy, *out.LifecyclePolicyText)
	assert.Equal(t, ecr.LifecyclePolicyPreviewStatusComplete, *out.Status)
}

func TestGetLifecyclePolicyPreview_ProjectsExpiryResults(t *testing.T) {
	catalog := fakeImageCatalog{records: []ecrregistry.ImageRecord{
		{Digest: "sha256:older", Tags: []string{"v1"}, PushedAt: time.Now().Add(-time.Hour)},
		{Digest: "sha256:newer", Tags: []string{"v2"}, PushedAt: time.Now()},
	}}
	encodedPolicy, err := json.Marshal(lifecyclePreviewPolicy)
	require.NoError(t, err)
	body := []byte(`{"repositoryName":"team/app","lifecyclePolicyText":` + string(encodedPolicy) + `}`)

	out, err := GetLifecyclePolicyPreview(context.Background(), fakeLifecyclePolicyStore{}, catalog, "123456789012", body)
	require.NoError(t, err)
	assert.Equal(t, ecr.LifecyclePolicyPreviewStatusComplete, *out.Status)
	require.Len(t, out.PreviewResults, 1)
	assert.Equal(t, "sha256:older", *out.PreviewResults[0].ImageDigest)
	assert.Equal(t, int64(7), *out.PreviewResults[0].AppliedRulePriority)
	require.NotNil(t, out.Summary)
	assert.Equal(t, int64(1), *out.Summary.ExpiringImageTotalCount)
}

func TestEvaluateLifecyclePreview_MapsPolicyAndRepositoryFailures(t *testing.T) {
	cases := []struct {
		name     string
		policies LifecyclePolicyStore
		catalog  ImageCatalog
		body     string
		code     string
	}{
		{"missing policy", fakeLifecyclePolicyStore{err: ecrdomain.ErrNotFound}, fakeImageCatalog{}, `{"repositoryName":"team/app"}`, awserrors.ErrorLifecyclePolicyNotFound},
		{"missing repository", fakeLifecyclePolicyStore{}, fakeImageCatalog{err: ecrdomain.ErrNotFound}, `{"repositoryName":"team/app","lifecyclePolicyText":"{}"}`, awserrors.ErrorRepositoryNotFound},
		{"invalid policy", fakeLifecyclePolicyStore{}, fakeImageCatalog{}, `{"repositoryName":"team/app","lifecyclePolicyText":"not-json"}`, awserrors.ErrorECRInvalidParameter},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := EvaluateLifecyclePreview(context.Background(), tc.policies, tc.catalog, "123456789012", []byte(tc.body))
			require.Error(t, err)
			assert.Equal(t, tc.code, awserrors.ValidErrorCodeFromError(err))
		})
	}
}
