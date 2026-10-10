package awsapi

import (
	"context"
	"errors"
	"testing"

	ecrregistry "github.com/mulgadc/spinifex/spinifex/domains/ecr/registry"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeManifestWriter struct {
	gotAccount, gotRepository, gotReference, gotContentType string
	gotBody                                                 []byte
	digest                                                  string
	err                                                     error
}

func (w *fakeManifestWriter) StoreManifest(_ context.Context, account, repository, reference, contentType string, body []byte) (string, error) {
	w.gotAccount, w.gotRepository, w.gotReference, w.gotContentType = account, repository, reference, contentType
	w.gotBody = body
	return w.digest, w.err
}

func TestPutImage_StoresManifestAndProjectsResult(t *testing.T) {
	writer := &fakeManifestWriter{digest: "sha256:stored"}
	body := []byte(`{"repositoryName":"team/app","imageManifest":"{\"schemaVersion\":2}","imageManifestMediaType":"application/vnd.oci.image.manifest.v1+json","imageTag":"v1","imageDigest":"sha256:ignored"}`)

	out, err := PutImage(context.Background(), writer, "123456789012", body)
	require.NoError(t, err)
	assert.Equal(t, "123456789012", writer.gotAccount)
	assert.Equal(t, "team/app", writer.gotRepository)
	assert.Equal(t, "v1", writer.gotReference)
	assert.Equal(t, []byte(`{"schemaVersion":2}`), writer.gotBody)
	assert.Equal(t, "sha256:stored", *out.Image.ImageId.ImageDigest)
	assert.Equal(t, "v1", *out.Image.ImageId.ImageTag)
}

func TestPutImage_ValidatesRequestAndMapsStoreErrors(t *testing.T) {
	cases := []struct {
		name   string
		writer ManifestWriter
		body   string
		code   string
	}{
		{"missing writer", nil, `{"repositoryName":"team/app","imageManifest":"{}"}`, awserrors.ErrorServerInternal},
		{"malformed", &fakeManifestWriter{}, `{`, awserrors.ErrorECRInvalidParameter},
		{"missing manifest", &fakeManifestWriter{}, `{"repositoryName":"team/app"}`, awserrors.ErrorECRInvalidParameter},
		{"cross account", &fakeManifestWriter{}, `{"repositoryName":"team/app","registryId":"999999999999","imageManifest":"{}"}`, awserrors.ErrorAccessDenied},
		{"digest mismatch", &fakeManifestWriter{err: &ecrregistry.ManifestStoreError{Code: "DIGEST_INVALID"}}, `{"repositoryName":"team/app","imageManifest":"{}"}`, awserrors.ErrorImageDigestDoesNotMatch},
		{"backend failure", &fakeManifestWriter{err: errors.New("unavailable")}, `{"repositoryName":"team/app","imageManifest":"{}"}`, awserrors.ErrorServerInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PutImage(context.Background(), tc.writer, "123456789012", []byte(tc.body))
			require.Error(t, err)
			assert.Equal(t, tc.code, awserrors.ValidErrorCodeFromError(err))
		})
	}
}
