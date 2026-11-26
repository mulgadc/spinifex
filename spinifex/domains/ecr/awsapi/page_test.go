package awsapi

import (
	"context"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/mulgadc/spinifex/spinifex/domains/ecr"
	ecrregistry "github.com/mulgadc/spinifex/spinifex/domains/ecr/registry"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// walkPages calls page with each returned token until none is left, and
// requires every page to hold at most size items and each before the last
// to be full.
func walkPages(t *testing.T, size int, page func(token string) ([]string, *string)) []string {
	t.Helper()
	var seen []string
	token := ""
	for range 10 {
		items, next := page(token)
		require.LessOrEqual(t, len(items), size, "a page must not exceed maxResults")
		seen = append(seen, items...)
		if next == nil {
			return seen
		}
		require.Len(t, items, size, "a page before the last must be full")
		token = *next
	}
	t.Fatal("paging did not end")
	return nil
}

func tokenField(token string) string {
	if token == "" {
		return ""
	}
	return fmt.Sprintf(`,"nextToken":%q`, token)
}

func TestDescribeRepositories_PagesByName(t *testing.T) {
	nc := newRepositoryActionTestConn(t)
	store := ecr.NewNATSMetaStore(nc)
	for _, name := range []string{"team/c", "team/a", "team/b"} {
		seedRepositoryForAction(t, nc, name)
	}

	seen := walkPages(t, 2, func(token string) ([]string, *string) {
		out, err := DescribeRepositories(context.Background(), store, repositoryActionEndpoint(), repositoryActionTestAccount,
			[]byte(`{"maxResults":2`+tokenField(token)+`}`))
		require.NoError(t, err)
		var names []string
		for _, repository := range out.Repositories {
			names = append(names, aws.StringValue(repository.RepositoryName))
		}
		return names, out.NextToken
	})
	assert.Equal(t, []string{"team/a", "team/b", "team/c"}, seen)
}

func TestDescribeImages_PagesByDigest(t *testing.T) {
	catalog := fakeImageCatalog{records: []ecrregistry.ImageRecord{
		{Digest: "sha256:c"}, {Digest: "sha256:a", Tags: []string{"v1"}}, {Digest: "sha256:b"},
	}}

	seen := walkPages(t, 2, func(token string) ([]string, *string) {
		out, err := DescribeImages(context.Background(), catalog, "123456789012",
			[]byte(`{"repositoryName":"team/app","maxResults":2`+tokenField(token)+`}`))
		require.NoError(t, err)
		var digests []string
		for _, detail := range out.ImageDetails {
			digests = append(digests, aws.StringValue(detail.ImageDigest))
		}
		return digests, out.NextToken
	})
	assert.Equal(t, []string{"sha256:a", "sha256:b", "sha256:c"}, seen)
}

func TestListImages_PagesAfterTagStatusFilter(t *testing.T) {
	// sha256:a is untagged and sorts first, so a page cut before the filter
	// would return a short first page.
	catalog := fakeImageCatalog{records: []ecrregistry.ImageRecord{
		{Digest: "sha256:c", Tags: []string{"v3"}},
		{Digest: "sha256:a"},
		{Digest: "sha256:b", Tags: []string{"v2", "latest"}},
	}}

	seen := walkPages(t, 2, func(token string) ([]string, *string) {
		out, err := ListImages(context.Background(), catalog, "123456789012",
			[]byte(`{"repositoryName":"team/app","maxResults":2,"filter":{"tagStatus":"TAGGED"}`+tokenField(token)+`}`))
		require.NoError(t, err)
		var ids []string
		for _, id := range out.ImageIds {
			ids = append(ids, aws.StringValue(id.ImageDigest)+"/"+aws.StringValue(id.ImageTag))
		}
		return ids, out.NextToken
	})
	assert.Equal(t, []string{"sha256:b/latest", "sha256:b/v2", "sha256:c/v3"}, seen)
}

func TestListImages_DefaultPageIs100(t *testing.T) {
	var records []ecrregistry.ImageRecord
	for i := range 101 {
		records = append(records, ecrregistry.ImageRecord{Digest: fmt.Sprintf("sha256:%03d", i)})
	}
	out, err := ListImages(context.Background(), fakeImageCatalog{records: records}, "123456789012", []byte(`{"repositoryName":"team/app"}`))
	require.NoError(t, err)
	assert.Len(t, out.ImageIds, 100)
	require.NotNil(t, out.NextToken)
}

func TestPagedActions_RefuseForeignTokenAndIDCombination(t *testing.T) {
	nc := newRepositoryActionTestConn(t)
	store := ecr.NewNATSMetaStore(nc)
	seedRepositoryForAction(t, nc, "team/app")
	catalog := fakeImageCatalog{records: []ecrregistry.ImageRecord{{Digest: "sha256:a", Tags: []string{"v1"}}}}

	describeRepositories := func(body string) error {
		_, err := DescribeRepositories(context.Background(), store, repositoryActionEndpoint(), repositoryActionTestAccount, []byte(body))
		return err
	}
	describeImages := func(body string) error {
		_, err := DescribeImages(context.Background(), catalog, "123456789012", []byte(body))
		return err
	}
	listImages := func(body string) error {
		_, err := ListImages(context.Background(), catalog, "123456789012", []byte(body))
		return err
	}

	cases := []struct {
		name    string
		call    func(string) error
		body    string
		message string
	}{
		{"DescribeRepositories foreign token", describeRepositories, `{"nextToken":"bm90LW91cnM"}`, "nextToken"},
		{"DescribeImages foreign token", describeImages, `{"repositoryName":"team/app","nextToken":"bm90LW91cnM"}`, "nextToken"},
		{"ListImages foreign token", listImages, `{"repositoryName":"team/app","nextToken":"bm90LW91cnM"}`, "nextToken"},
		{"DescribeRepositories maxResults with names", describeRepositories, `{"repositoryNames":["team/app"],"maxResults":1}`, "repositoryNames"},
		{"DescribeRepositories nextToken with names", describeRepositories, `{"repositoryNames":["team/app"],"nextToken":"x"}`, "repositoryNames"},
		{"DescribeImages maxResults with ids", describeImages, `{"repositoryName":"team/app","imageIds":[{"imageTag":"v1"}],"maxResults":1}`, "imageIds"},
		{"DescribeImages nextToken with ids", describeImages, `{"repositoryName":"team/app","imageIds":[{"imageTag":"v1"}],"nextToken":"x"}`, "imageIds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(tc.body)
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorECRInvalidParameter, awserrors.ValidErrorCodeFromError(err))
			assert.Contains(t, err.Error(), tc.message)
		})
	}
}
