package awsapi

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/service/ecr"
	ecrregistry "github.com/mulgadc/spinifex/spinifex/domains/ecr/registry"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeImageDeleter struct {
	results map[string]fakeDeleteResult
}

type fakeDeleteResult struct {
	digest string
	err    error
}

func (d fakeImageDeleter) DeleteImage(_ context.Context, _ string, _ string, tag, digest string) (string, error) {
	reference := digest
	if reference == "" {
		reference = tag
	}
	result, ok := d.results[reference]
	if !ok {
		return "", ecrregistry.ErrImageNotFound
	}
	return result.digest, result.err
}

func TestBatchDeleteImage_ReturnsDeletedImagesAndPerItemFailures(t *testing.T) {
	deleter := fakeImageDeleter{results: map[string]fakeDeleteResult{
		"sha256:present": {digest: "sha256:present"},
	}}
	body := []byte(`{"repositoryName":"team/app","imageIds":[{"imageDigest":"sha256:present"},{"imageTag":"missing"},{}]}`)

	out, err := BatchDeleteImage(context.Background(), deleter, "123456789012", body)
	require.NoError(t, err)
	require.Len(t, out.ImageIds, 1)
	assert.Equal(t, "sha256:present", *out.ImageIds[0].ImageDigest)
	require.Len(t, out.Failures, 2)
	assert.Equal(t, ecr.ImageFailureCodeImageNotFound, *out.Failures[0].FailureCode)
	assert.Equal(t, ecr.ImageFailureCodeMissingDigestAndTag, *out.Failures[1].FailureCode)
}

func TestBatchDeleteImage_ValidatesRequestAndDeleterFailure(t *testing.T) {
	tooMany := `{"repositoryName":"team/app","imageIds":[` + repeatImageIdentifier(101) + `]}`
	cases := []struct {
		name    string
		deleter ImageDeleter
		body    string
		code    string
	}{
		{"missing deleter", nil, `{"repositoryName":"team/app"}`, awserrors.ErrorServerInternal},
		{"malformed", fakeImageDeleter{}, `{`, awserrors.ErrorInvalidParameterValue},
		{"cross account", fakeImageDeleter{}, `{"repositoryName":"team/app","registryId":"999999999999"}`, awserrors.ErrorAccessDenied},
		{"cap exceeded", fakeImageDeleter{}, tooMany, awserrors.ErrorInvalidParameterValue},
		{"backend failure", fakeImageDeleter{results: map[string]fakeDeleteResult{"present": {err: errors.New("unavailable")}}}, `{"repositoryName":"team/app","imageIds":[{"imageTag":"present"}]}`, awserrors.ErrorServerInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := BatchDeleteImage(context.Background(), tc.deleter, "123456789012", []byte(tc.body))
			require.Error(t, err)
			assert.Equal(t, tc.code, awserrors.ValidErrorCodeFromError(err))
		})
	}
}
