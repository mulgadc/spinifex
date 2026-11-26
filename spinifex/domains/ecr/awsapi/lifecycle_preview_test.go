package awsapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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
		{"missing repository", fakeLifecyclePolicyStore{}, fakeImageCatalog{err: ecrdomain.ErrNotFound}, `{"repositoryName":"team/app","lifecyclePolicyText":` + strconvQuote(lifecyclePreviewPolicy) + `}`, awserrors.ErrorRepositoryNotFound},
		{"invalid policy", fakeLifecyclePolicyStore{}, fakeImageCatalog{}, `{"repositoryName":"team/app","lifecyclePolicyText":` + strconvQuote("not-json"+strings.Repeat(" ", 100)) + `}`, awserrors.ErrorECRInvalidParameter},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := EvaluateLifecyclePreview(context.Background(), tc.policies, tc.catalog, "123456789012", []byte(tc.body))
			require.Error(t, err)
			assert.Equal(t, tc.code, awserrors.ValidErrorCodeFromError(err))
		})
	}
}

// A parseable policy outside the model's 100-30720 characters is refused on
// length, and an omitted text still falls back to the stored policy.
func TestEvaluateLifecyclePreview_PolicyTextLength(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		message string
	}{
		{"too short", `{"rules":[]}`, "Invalid parameter at 'lifecyclePolicyText' failed to satisfy constraint: 'Member must have length greater than or equal to 100'"},
		{"too long", lifecyclePreviewPolicy + strings.Repeat(" ", 30720), "Invalid parameter at 'lifecyclePolicyText' failed to satisfy constraint: 'Member must have length less than or equal to 30720'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"repositoryName":"team/app","lifecyclePolicyText":` + strconvQuote(tc.text) + `}`)
			_, err := EvaluateLifecyclePreview(context.Background(), fakeLifecyclePolicyStore{}, fakeImageCatalog{}, "123456789012", body)
			code, message, ok := awserrors.ResolveErrorDetail(err)
			require.True(t, ok)
			assert.Equal(t, awserrors.ErrorECRInvalidParameter, code)
			assert.Equal(t, tc.message, message)
		})
	}
}

// previewCatalog holds five expiring images and one kept newest image under
// lifecyclePreviewPolicy. sha256:a is the only untagged one.
func previewCatalog() fakeImageCatalog {
	now := time.Now()
	return fakeImageCatalog{records: []ecrregistry.ImageRecord{
		{Digest: "sha256:a", PushedAt: now.Add(-6 * time.Hour)},
		{Digest: "sha256:b", Tags: []string{"b"}, PushedAt: now.Add(-5 * time.Hour)},
		{Digest: "sha256:c", Tags: []string{"c"}, PushedAt: now.Add(-4 * time.Hour)},
		{Digest: "sha256:d", Tags: []string{"d"}, PushedAt: now.Add(-3 * time.Hour)},
		{Digest: "sha256:e", Tags: []string{"e"}, PushedAt: now.Add(-2 * time.Hour)},
		{Digest: "sha256:f", Tags: []string{"f"}, PushedAt: now},
	}}
}

func previewBody(extra string) []byte {
	return []byte(`{"repositoryName":"team/app","lifecyclePolicyText":` + strconvQuote(lifecyclePreviewPolicy) + extra + `}`)
}

func previewDigests(out *ecr.GetLifecyclePolicyPreviewOutput) []string {
	digests := make([]string, 0, len(out.PreviewResults))
	for _, r := range out.PreviewResults {
		digests = append(digests, *r.ImageDigest)
	}
	return digests
}

func TestGetLifecyclePolicyPreview_PagesAfterFilter(t *testing.T) {
	var pages [][]string
	token := ""
	for {
		extra := `,"maxResults":2,"filter":{"tagStatus":"TAGGED"}`
		if token != "" {
			extra += `,"nextToken":` + strconvQuote(token)
		}
		out, err := GetLifecyclePolicyPreview(context.Background(), fakeLifecyclePolicyStore{}, previewCatalog(), "123456789012", previewBody(extra))
		require.NoError(t, err)
		assert.Equal(t, int64(5), *out.Summary.ExpiringImageTotalCount, "summary counts every expiring image")
		pages = append(pages, previewDigests(out))
		if out.NextToken == nil {
			break
		}
		token = *out.NextToken
		require.Less(t, len(pages), 5, "page walk does not end")
	}
	assert.Equal(t, [][]string{{"sha256:b", "sha256:c"}, {"sha256:d", "sha256:e"}}, pages)
}

func TestGetLifecyclePolicyPreview_DefaultPageIsOneHundred(t *testing.T) {
	now := time.Now()
	records := make([]ecrregistry.ImageRecord, 0, 102)
	for i := range 102 {
		records = append(records, ecrregistry.ImageRecord{Digest: fmt.Sprintf("sha256:%03d", i), PushedAt: now.Add(time.Duration(i) * time.Minute)})
	}
	out, err := GetLifecyclePolicyPreview(context.Background(), fakeLifecyclePolicyStore{}, fakeImageCatalog{records: records}, "123456789012", previewBody(""))
	require.NoError(t, err)
	assert.Len(t, out.PreviewResults, 100)
	assert.NotNil(t, out.NextToken)
	assert.Equal(t, int64(101), *out.Summary.ExpiringImageTotalCount)
}

func TestGetLifecyclePolicyPreview_Filters(t *testing.T) {
	cases := []struct {
		name  string
		extra string
		want  []string
	}{
		{"untagged", `,"filter":{"tagStatus":"UNTAGGED"}`, []string{"sha256:a"}},
		{"any", `,"filter":{"tagStatus":"ANY"}`, []string{"sha256:a", "sha256:b", "sha256:c", "sha256:d", "sha256:e"}},
		{"image ids", `,"imageIds":[{"imageDigest":"sha256:c"},{"imageTag":"e"},{"imageTag":"f"}]`, []string{"sha256:c", "sha256:e"}},
		{"image ids and tag status", `,"imageIds":[{"imageDigest":"sha256:a"},{"imageDigest":"sha256:b"}],"filter":{"tagStatus":"TAGGED"}`, []string{"sha256:b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := GetLifecyclePolicyPreview(context.Background(), fakeLifecyclePolicyStore{}, previewCatalog(), "123456789012", previewBody(tc.extra))
			require.NoError(t, err)
			assert.Equal(t, tc.want, previewDigests(out))
			assert.Nil(t, out.NextToken)
			assert.Equal(t, int64(5), *out.Summary.ExpiringImageTotalCount)
		})
	}
}

func TestGetLifecyclePolicyPreview_RefusesBadPagingAndFilter(t *testing.T) {
	cases := []struct {
		name  string
		extra string
	}{
		{"zero maxResults", `,"maxResults":0`},
		{"maxResults above 100", `,"maxResults":101`},
		{"foreign token", `,"nextToken":"bm90LW91cnM"`},
		{"undecodable token", `,"nextToken":"!!!"`},
		{"bad tagStatus", `,"filter":{"tagStatus":"SOME"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := GetLifecyclePolicyPreview(context.Background(), fakeLifecyclePolicyStore{}, previewCatalog(), "123456789012", previewBody(tc.extra))
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorECRInvalidParameter, awserrors.ValidErrorCodeFromError(err))
		})
	}
}
