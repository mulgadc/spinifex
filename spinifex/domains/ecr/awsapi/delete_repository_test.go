package awsapi

import (
	"context"
	"strings"
	"testing"
	"time"

	handlers_ecr "github.com/mulgadc/spinifex/spinifex/domains/ecr"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeleteRepository_RemovesEmptyRepository(t *testing.T) {
	nc := newRepositoryActionTestConn(t)
	store := handlers_ecr.NewNATSMetaStore(nc)
	seedRepositoryForAction(t, nc, "team/app")

	out, err := DeleteRepository(context.Background(), store, repositoryActionEndpoint(), repositoryActionTestAccount, []byte(`{"repositoryName":"team/app"}`))
	require.NoError(t, err)
	require.NotNil(t, out.Repository)
	assert.Equal(t, "team/app", *out.Repository.RepositoryName)

	_, err = store.GetRepo(context.Background(), repositoryActionTestAccount, "team/app")
	require.ErrorIs(t, err, handlers_ecr.ErrNotFound)
}

func TestDeleteRepository_ProtectsNonEmptyRepositoryUnlessForced(t *testing.T) {
	nc := newRepositoryActionTestConn(t)
	seedRepositoryForAction(t, nc, "team/app")
	store := handlers_ecr.NewNATSMetaStore(nc)
	require.NoError(t, store.PutManifestMeta(context.Background(), repositoryActionTestAccount, "team/app", handlers_ecr.ManifestMeta{
		Digest: "sha256:" + strings.Repeat("a", 64), MediaType: "application/json", Size: 7, PushedAt: time.Now(),
	}))

	_, err := DeleteRepository(context.Background(), store, repositoryActionEndpoint(), repositoryActionTestAccount, []byte(`{"repositoryName":"team/app"}`))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorRepositoryNotEmpty, awserrors.ValidErrorCodeFromError(err))

	out, err := DeleteRepository(context.Background(), store, repositoryActionEndpoint(), repositoryActionTestAccount, []byte(`{"repositoryName":"team/app","force":true}`))
	require.NoError(t, err)
	assert.Equal(t, "team/app", *out.Repository.RepositoryName)
}

func TestDeleteRepository_ValidatesRequest(t *testing.T) {
	nc := newRepositoryActionTestConn(t)
	store := handlers_ecr.NewNATSMetaStore(nc)
	cases := []struct {
		name string
		body string
		code string
	}{
		{"malformed", `{`, awserrors.ErrorInvalidParameterValue},
		{"missing name", `{}`, awserrors.ErrorInvalidParameterValue},
		{"cross account", `{"repositoryName":"team/app","registryId":"999999999999"}`, awserrors.ErrorAccessDenied},
		{"missing repository", `{"repositoryName":"team/missing"}`, awserrors.ErrorRepositoryNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DeleteRepository(context.Background(), store, repositoryActionEndpoint(), repositoryActionTestAccount, []byte(tc.body))
			require.Error(t, err)
			assert.Equal(t, tc.code, awserrors.ValidErrorCodeFromError(err))
		})
	}
}
