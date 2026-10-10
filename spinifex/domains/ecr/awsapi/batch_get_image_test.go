package awsapi

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/service/ecr"
	ecrregistry "github.com/mulgadc/spinifex/spinifex/domains/ecr/registry"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeManifestReader struct {
	manifests map[string]fakeManifest
}

type fakeManifest struct {
	body, mediaType, digest string
	err                     error
}

func (r fakeManifestReader) GetManifest(_ context.Context, _ string, _ string, reference string, _ []string) ([]byte, string, string, error) {
	manifest, ok := r.manifests[reference]
	if !ok {
		return nil, "", "", ecrregistry.ErrImageNotFound
	}
	return []byte(manifest.body), manifest.mediaType, manifest.digest, manifest.err
}

func TestBatchGetImage_ReturnsImagesAndPerItemFailures(t *testing.T) {
	reader := fakeManifestReader{manifests: map[string]fakeManifest{
		"sha256:present": {body: `{"schemaVersion":2}`, mediaType: "application/vnd.oci.image.manifest.v1+json", digest: "sha256:present"},
	}}
	body := []byte(`{"repositoryName":"team/app","imageIds":[{"imageDigest":"sha256:present","imageTag":"v1"},{"imageTag":"missing"},{}]}`)

	out, err := BatchGetImage(context.Background(), reader, "123456789012", body)
	require.NoError(t, err)
	require.Len(t, out.Images, 1)
	assert.Equal(t, "sha256:present", *out.Images[0].ImageId.ImageDigest)
	assert.Equal(t, "v1", *out.Images[0].ImageId.ImageTag)
	require.Len(t, out.Failures, 2)
	assert.Equal(t, ecr.ImageFailureCodeImageNotFound, *out.Failures[0].FailureCode)
	assert.Equal(t, ecr.ImageFailureCodeMissingDigestAndTag, *out.Failures[1].FailureCode)
}

func TestBatchGetImage_ValidatesRequestAndReaderFailure(t *testing.T) {
	tooMany := `{"repositoryName":"team/app","imageIds":[` + repeatImageIdentifier(101) + `]}`
	cases := []struct {
		name   string
		reader ManifestReader
		body   string
		code   string
	}{
		{"missing reader", nil, `{"repositoryName":"team/app"}`, awserrors.ErrorServerInternal},
		{"malformed", fakeManifestReader{}, `{`, awserrors.ErrorECRInvalidParameter},
		{"cross account", fakeManifestReader{}, `{"repositoryName":"team/app","registryId":"999999999999"}`, awserrors.ErrorAccessDenied},
		{"cap exceeded", fakeManifestReader{}, tooMany, awserrors.ErrorECRInvalidParameter},
		{"backend failure", fakeManifestReader{manifests: map[string]fakeManifest{"present": {err: errors.New("unavailable")}}}, `{"repositoryName":"team/app","imageIds":[{"imageTag":"present"}]}`, awserrors.ErrorServerInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := BatchGetImage(context.Background(), tc.reader, "123456789012", []byte(tc.body))
			require.Error(t, err)
			assert.Equal(t, tc.code, awserrors.ValidErrorCodeFromError(err))
		})
	}
}

func repeatImageIdentifier(count int) string {
	items := make([]string, count)
	for i := range items {
		items[i] = `{"imageTag":"t"}`
	}
	return strings.Join(items, ",")
}
