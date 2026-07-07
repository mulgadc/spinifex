package gateway_ecrapi

import (
	"context"

	"testing"

	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ecr "github.com/mulgadc/spinifex/spinifex/handlers/ecr"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validLifecyclePolicy = `{"rules":[{"rulePriority":1,"selection":{"tagStatus":"untagged","countType":"sinceImagePushed","countUnit":"days","countNumber":14},"action":{"type":"expire"}}]}`

// newLifecycleTestConn starts an embedded JetStream server with the repo +
// lifecycle metadata subjects served, returning the gateway NATS connection.
func newLifecycleTestConn(t *testing.T) *nats.Conn {
	t.Helper()
	_, nc, js := testutil.StartTestJetStream(t)
	svc := handlers_ecr.NewKVMetaService(js)
	serveMeta(t, nc, handlers_ecr.SubjectRepoCreate, svc.RepoCreate)
	serveMeta(t, nc, handlers_ecr.SubjectRepoDescribe, svc.RepoDescribe)
	serveMeta(t, nc, handlers_ecr.SubjectLifecyclePut, svc.LifecyclePut)
	serveMeta(t, nc, handlers_ecr.SubjectLifecycleGet, svc.LifecycleGet)
	serveMeta(t, nc, handlers_ecr.SubjectLifecycleDelete, svc.LifecycleDelete)
	return nc
}

func TestLifecyclePolicy_Lifecycle(t *testing.T) {
	nc := newLifecycleTestConn(t)
	seedRepo(t, nc, "team/app")
	body := []byte(`{"repositoryName":"team/app","lifecyclePolicyText":` + strconvQuote(validLifecyclePolicy) + `}`)

	out, err := PutLifecyclePolicy(context.Background(), nc, policyTestAccount, body)
	require.NoError(t, err)
	put, ok := out.(*ecr.PutLifecyclePolicyOutput)
	require.True(t, ok)
	assert.Equal(t, validLifecyclePolicy, *put.LifecyclePolicyText)
	assert.Equal(t, "team/app", *put.RepositoryName)
	assert.Equal(t, policyTestAccount, *put.RegistryId)

	out, err = GetLifecyclePolicy(context.Background(), nc, policyTestAccount, []byte(`{"repositoryName":"team/app"}`))
	require.NoError(t, err)
	got, ok := out.(*ecr.GetLifecyclePolicyOutput)
	require.True(t, ok)
	assert.Equal(t, validLifecyclePolicy, *got.LifecyclePolicyText)

	out, err = DeleteLifecyclePolicy(context.Background(), nc, policyTestAccount, []byte(`{"repositoryName":"team/app"}`))
	require.NoError(t, err)
	del, ok := out.(*ecr.DeleteLifecyclePolicyOutput)
	require.True(t, ok)
	assert.Equal(t, validLifecyclePolicy, *del.LifecyclePolicyText)

	_, err = GetLifecyclePolicy(context.Background(), nc, policyTestAccount, []byte(`{"repositoryName":"team/app"}`))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorLifecyclePolicyNotFound, err.Error())
}

func TestLifecyclePolicy_Errors(t *testing.T) {
	nc := newLifecycleTestConn(t)
	seedRepo(t, nc, "team/app")

	cases := []struct {
		name   string
		fn     func(context.Context, *nats.Conn, string, []byte) (any, error)
		body   string
		expect string
	}{
		{"put missing repo", PutLifecyclePolicy, `{"repositoryName":"team/ghost","lifecyclePolicyText":` + strconvQuote(validLifecyclePolicy) + `}`, awserrors.ErrorRepositoryNotFound},
		{"put invalid json", PutLifecyclePolicy, `{"repositoryName":"team/app","lifecyclePolicyText":"not-json"}`, awserrors.ErrorInvalidParameterValue},
		{"put bad rule", PutLifecyclePolicy, `{"repositoryName":"team/app","lifecyclePolicyText":"{\"rules\":[{\"rulePriority\":1,\"selection\":{\"tagStatus\":\"tagged\",\"countType\":\"imageCountMoreThan\",\"countNumber\":1},\"action\":{\"type\":\"expire\"}}]}"}`, awserrors.ErrorInvalidParameterValue},
		{"put empty name", PutLifecyclePolicy, `{"lifecyclePolicyText":` + strconvQuote(validLifecyclePolicy) + `}`, awserrors.ErrorInvalidParameterValue},
		{"put cross-account", PutLifecyclePolicy, `{"repositoryName":"team/app","registryId":"999999999999","lifecyclePolicyText":` + strconvQuote(validLifecyclePolicy) + `}`, awserrors.ErrorAccessDenied},
		{"get no policy", GetLifecyclePolicy, `{"repositoryName":"team/app"}`, awserrors.ErrorLifecyclePolicyNotFound},
		{"delete no policy", DeleteLifecyclePolicy, `{"repositoryName":"team/app"}`, awserrors.ErrorLifecyclePolicyNotFound},
		{"get cross-account", GetLifecyclePolicy, `{"repositoryName":"team/app","registryId":"999999999999"}`, awserrors.ErrorAccessDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.fn(context.Background(), nc, policyTestAccount, []byte(tc.body))
			require.Error(t, err)
			assert.Equal(t, tc.expect, err.Error())
		})
	}
}
