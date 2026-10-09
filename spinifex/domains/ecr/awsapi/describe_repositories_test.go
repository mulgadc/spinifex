package awsapi

import (
	"context"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/ecr"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDescribeRepositories_ListsAccountScoped(t *testing.T) {
	nc := newRepositoryActionTestConn(t)
	store := ecr.NewNATSMetaStore(nc)
	seedRepositoryForAction(t, nc, "team/app")
	seedRepositoryForAction(t, nc, "team/web")

	out, err := DescribeRepositories(context.Background(), store, repositoryActionEndpoint(), repositoryActionTestAccount, []byte(`{}`))
	require.NoError(t, err)
	require.Len(t, out.Repositories, 2)

	byName := map[string]int{}
	for i, repository := range out.Repositories {
		byName[*repository.RepositoryName] = i
	}
	app := out.Repositories[byName["team/app"]]
	assert.Equal(t, repositoryActionTestAccount, *app.RegistryId)
	assert.Equal(t, "arn:aws:ecr:ap-southeast-2:"+repositoryActionTestAccount+":repository/team/app", *app.RepositoryArn)
	assert.Equal(t, repositoryActionTestAccount+".dkr.ecr.ap-southeast-2.spinifex.test/team/app", *app.RepositoryUri)
}

func TestDescribeRepositories_ValidatesRequestAndRepository(t *testing.T) {
	nc := newRepositoryActionTestConn(t)
	store := ecr.NewNATSMetaStore(nc)
	seedRepositoryForAction(t, nc, "team/app")

	cases := []struct {
		name string
		body string
		code string
	}{
		{"malformed body", `{`, awserrors.ErrorECRInvalidParameter},
		{"cross-account registry", `{"registryId":"999999999999"}`, awserrors.ErrorAccessDenied},
		{"missing named repository", `{"repositoryNames":["team/ghost"]}`, awserrors.ErrorRepositoryNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DescribeRepositories(context.Background(), store, repositoryActionEndpoint(), repositoryActionTestAccount, []byte(tc.body))
			require.Error(t, err)
			assert.Equal(t, tc.code, awserrors.ValidErrorCodeFromError(err))
		})
	}
}
