package awsapi

import (
	"context"
	"errors"
	"testing"

	handlers_ecr "github.com/mulgadc/spinifex/spinifex/domains/ecr"
	ecrregistry "github.com/mulgadc/spinifex/spinifex/domains/ecr/registry"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeImageCatalog struct {
	records []ecrregistry.ImageRecord
	err     error
}

func (c fakeImageCatalog) ListImages(context.Context, string, string) ([]ecrregistry.ImageRecord, error) {
	return c.records, c.err
}

func TestListImages_ProjectsTaggedAndUntaggedRecords(t *testing.T) {
	catalog := fakeImageCatalog{records: []ecrregistry.ImageRecord{
		{Digest: "sha256:tagged", Tags: []string{"v1", "latest"}},
		{Digest: "sha256:untagged"},
	}}

	out, err := ListImages(context.Background(), catalog, "123456789012", []byte(`{"repositoryName":"team/app"}`))
	require.NoError(t, err)
	require.Len(t, out.ImageIds, 3)
	assert.Equal(t, "sha256:tagged", *out.ImageIds[0].ImageDigest)
	assert.Equal(t, "v1", *out.ImageIds[0].ImageTag)
	assert.Equal(t, "sha256:untagged", *out.ImageIds[2].ImageDigest)
	assert.Nil(t, out.ImageIds[2].ImageTag)

	out, err = ListImages(context.Background(), catalog, "123456789012", []byte(`{"repositoryName":"team/app","filter":{"tagStatus":"UNTAGGED"}}`))
	require.NoError(t, err)
	require.Len(t, out.ImageIds, 1)
	assert.Equal(t, "sha256:untagged", *out.ImageIds[0].ImageDigest)
}

func TestListImages_ValidatesRequestAndCatalogFailure(t *testing.T) {
	cases := []struct {
		name    string
		catalog ImageCatalog
		body    string
		code    string
	}{
		{"missing catalog", nil, `{"repositoryName":"team/app"}`, awserrors.ErrorServerInternal},
		{"malformed", fakeImageCatalog{}, `{`, awserrors.ErrorECRInvalidParameter},
		{"missing repository name", fakeImageCatalog{}, `{}`, awserrors.ErrorECRInvalidParameter},
		{"cross account", fakeImageCatalog{}, `{"repositoryName":"team/app","registryId":"999999999999"}`, awserrors.ErrorAccessDenied},
		{"repository missing", fakeImageCatalog{err: handlers_ecr.ErrNotFound}, `{"repositoryName":"team/app"}`, awserrors.ErrorRepositoryNotFound},
		{"backend failure", fakeImageCatalog{err: errors.New("unavailable")}, `{"repositoryName":"team/app"}`, awserrors.ErrorServerInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ListImages(context.Background(), tc.catalog, "123456789012", []byte(tc.body))
			require.Error(t, err)
			assert.Equal(t, tc.code, awserrors.ValidErrorCodeFromError(err))
		})
	}
}
