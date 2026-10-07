package awsapi

import (
	"context"
	"testing"

	handlers_ecr "github.com/mulgadc/spinifex/spinifex/domains/ecr"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateRepository_PersistsConfiguredMetadata(t *testing.T) {
	nc := newRepositoryActionTestConn(t)
	store := handlers_ecr.NewNATSMetaStore(nc)
	body := []byte(`{"repositoryName":"team/app","imageTagMutability":"IMMUTABLE","tags":[{"Key":"environment","Value":"test"}],"imageScanningConfiguration":{"scanOnPush":true}}`)

	out, err := CreateRepository(context.Background(), store, repositoryActionEndpoint(), repositoryActionTestAccount, body)
	require.NoError(t, err)
	require.NotNil(t, out.Repository)
	assert.Equal(t, "team/app", *out.Repository.RepositoryName)
	assert.Equal(t, "IMMUTABLE", *out.Repository.ImageTagMutability)
	assert.True(t, *out.Repository.ImageScanningConfiguration.ScanOnPush)

	meta, err := store.GetRepo(context.Background(), repositoryActionTestAccount, "team/app")
	require.NoError(t, err)
	assert.Equal(t, handlers_ecr.TagMutabilityImmutable, meta.ImageTagMutability)
	assert.Equal(t, map[string]string{"environment": "test"}, meta.Tags)
	assert.True(t, meta.ScanOnPush)
}

func TestCreateRepository_RejectsInvalidRequestsAndDuplicates(t *testing.T) {
	nc := newRepositoryActionTestConn(t)
	store := handlers_ecr.NewNATSMetaStore(nc)
	_, err := CreateRepository(context.Background(), store, repositoryActionEndpoint(), repositoryActionTestAccount, []byte(`{"repositoryName":"team/app"}`))
	require.NoError(t, err)

	cases := []struct {
		name string
		body string
		code string
	}{
		{"duplicate", `{"repositoryName":"team/app"}`, awserrors.ErrorRepositoryAlreadyExists},
		{"malformed", `{`, awserrors.ErrorECRInvalidParameter},
		{"missing name", `{}`, awserrors.ErrorECRInvalidParameter},
		{"cross account", `{"repositoryName":"team/other","registryId":"999999999999"}`, awserrors.ErrorAccessDenied},
		{"unsupported KMS", `{"repositoryName":"team/other","encryptionConfiguration":{"encryptionType":"KMS"}}`, awserrors.ErrorECRInvalidParameter},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CreateRepository(context.Background(), store, repositoryActionEndpoint(), repositoryActionTestAccount, []byte(tc.body))
			require.Error(t, err)
			assert.Equal(t, tc.code, awserrors.ValidErrorCodeFromError(err))
		})
	}
}
