package awsapi

import (
	"context"
	"testing"
	"time"

	ecrregistry "github.com/mulgadc/spinifex/spinifex/domains/ecr/registry"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDescribeImages_ProjectsAndSelectsImageDetails(t *testing.T) {
	pushedAt := time.Date(2026, 10, 1, 1, 2, 3, 0, time.UTC)
	catalog := fakeImageCatalog{records: []ecrregistry.ImageRecord{
		{Digest: "sha256:v1", Tags: []string{"v1"}, Size: 17, PushedAt: pushedAt, MediaType: "application/vnd.oci.image.manifest.v1+json"},
		{Digest: "sha256:v2", Tags: []string{"v2"}, Size: 19, MediaType: "application/vnd.oci.image.manifest.v1+json"},
	}}

	out, err := DescribeImages(context.Background(), catalog, "123456789012", []byte(`{"repositoryName":"team/app","imageIds":[{"imageTag":"v1"}]}`))
	require.NoError(t, err)
	require.Len(t, out.ImageDetails, 1)
	detail := out.ImageDetails[0]
	assert.Equal(t, "sha256:v1", *detail.ImageDigest)
	assert.Equal(t, []string{"v1"}, []string{*detail.ImageTags[0]})
	assert.Equal(t, int64(17), *detail.ImageSizeInBytes)
	assert.Equal(t, pushedAt, *detail.ImagePushedAt)
}

func TestDescribeImages_ReportsMissingRequestedImage(t *testing.T) {
	catalog := fakeImageCatalog{records: []ecrregistry.ImageRecord{{Digest: "sha256:present", Tags: []string{"present"}}}}
	_, err := DescribeImages(context.Background(), catalog, "123456789012", []byte(`{"repositoryName":"team/app","imageIds":[{"imageTag":"missing"}]}`))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorImageNotFound, awserrors.ValidErrorCodeFromError(err))
}
