package awsapi

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/service/ecr"
	ecrdomain "github.com/mulgadc/spinifex/spinifex/domains/ecr"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPutImageTagMutability_UpdatesRepository(t *testing.T) {
	nc := newRepositoryActionTestConn(t)
	seedRepositoryForAction(t, nc, "team/app")

	out, err := PutImageTagMutability(context.Background(), nc, repositoryActionTestAccount,
		[]byte(`{"repositoryName":"team/app","imageTagMutability":"IMMUTABLE"}`))
	require.NoError(t, err)
	response, ok := out.(*ecr.PutImageTagMutabilityOutput)
	require.True(t, ok)
	assert.Equal(t, "IMMUTABLE", *response.ImageTagMutability)

	meta, err := ecrdomain.NewNATSMetaStore(nc).GetRepo(context.Background(), repositoryActionTestAccount, "team/app")
	require.NoError(t, err)
	assert.Equal(t, ecrdomain.TagMutabilityImmutable, meta.ImageTagMutability)
}

func TestPutImageTagMutability_ValidatesRequest(t *testing.T) {
	nc := newRepositoryActionTestConn(t)
	seedRepositoryForAction(t, nc, "team/app")

	cases := []struct {
		name string
		body string
		code string
	}{
		{"malformed", `{`, awserrors.ErrorECRInvalidParameter},
		{"missing value", `{"repositoryName":"team/app"}`, awserrors.ErrorECRInvalidParameter},
		{"invalid value", `{"repositoryName":"team/app","imageTagMutability":"NOPE"}`, awserrors.ErrorECRInvalidParameter},
		{"cross account", `{"repositoryName":"team/app","registryId":"999999999999","imageTagMutability":"IMMUTABLE"}`, awserrors.ErrorAccessDenied},
		{"missing repository", `{"repositoryName":"team/missing","imageTagMutability":"IMMUTABLE"}`, awserrors.ErrorRepositoryNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PutImageTagMutability(context.Background(), nc, repositoryActionTestAccount, []byte(tc.body))
			require.Error(t, err)
			assert.Equal(t, tc.code, awserrors.ValidErrorCodeFromError(err))
		})
	}
}

func TestActions_PutImageTagMutabilityDispatched(t *testing.T) {
	handler, ok := Actions["PutImageTagMutability"]
	require.True(t, ok)
	nc := newRepositoryActionTestConn(t)
	seedRepositoryForAction(t, nc, "team/app")

	out, err := handler(context.Background(), nc, repositoryActionTestAccount,
		[]byte(`{"repositoryName":"team/app","imageTagMutability":"IMMUTABLE"}`))
	require.NoError(t, err)
	_, ok = out.(*ecr.PutImageTagMutabilityOutput)
	assert.True(t, ok)
}
