package gateway_ecrapi

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

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
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
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
	assert.Equal(t, awserrors.ErrorLifecyclePolicyNotFound, awserrors.ValidErrorCodeFromError(err))
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
		{"put invalid json", PutLifecyclePolicy, `{"repositoryName":"team/app","lifecyclePolicyText":"not-json"}`, awserrors.ErrorECRInvalidParameter},
		{"put bad rule", PutLifecyclePolicy, `{"repositoryName":"team/app","lifecyclePolicyText":"{\"rules\":[{\"rulePriority\":1,\"selection\":{\"tagStatus\":\"tagged\",\"countType\":\"imageCountMoreThan\",\"countNumber\":1},\"action\":{\"type\":\"expire\"}}]}"}`, awserrors.ErrorECRInvalidParameter},
		{"put empty name", PutLifecyclePolicy, `{"lifecyclePolicyText":` + strconvQuote(validLifecyclePolicy) + `}`, awserrors.ErrorECRInvalidParameter},
		{"put cross-account", PutLifecyclePolicy, `{"repositoryName":"team/app","registryId":"999999999999","lifecyclePolicyText":` + strconvQuote(validLifecyclePolicy) + `}`, awserrors.ErrorAccessDenied},
		{"get no policy", GetLifecyclePolicy, `{"repositoryName":"team/app"}`, awserrors.ErrorLifecyclePolicyNotFound},
		{"delete no policy", DeleteLifecyclePolicy, `{"repositoryName":"team/app"}`, awserrors.ErrorLifecyclePolicyNotFound},
		{"get cross-account", GetLifecyclePolicy, `{"repositoryName":"team/app","registryId":"999999999999"}`, awserrors.ErrorAccessDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.fn(context.Background(), nc, policyTestAccount, []byte(tc.body))
			require.Error(t, err)
			assert.Equal(t, tc.expect, awserrors.ValidErrorCodeFromError(err))
		})
	}
}

// AWS returns lifecyclePolicyText compacted with the submitted key order, and
// lastEvaluatedAt (epoch 0 before any evaluation) from Get and Delete only.
func TestLifecyclePolicy_CompactTextAndLastEvaluatedAt(t *testing.T) {
	nc := newLifecycleTestConn(t)
	seedRepo(t, nc, "team/app")
	pretty := "{\n  \"rules\" : [ {\n    \"rulePriority\" : 1,\n    \"selection\" : {\"tagStatus\": \"untagged\", \"countType\": \"sinceImagePushed\", \"countUnit\": \"days\", \"countNumber\": 14},\n    \"action\" : {\"type\": \"expire\"}\n  } ]\n}"
	body := []byte(`{"repositoryName":"team/app","lifecyclePolicyText":` + strconvQuote(pretty) + `}`)

	out, err := PutLifecyclePolicy(context.Background(), nc, policyTestAccount, body)
	require.NoError(t, err)
	put, ok := out.(*ecr.PutLifecyclePolicyOutput)
	require.True(t, ok)
	assert.Equal(t, validLifecyclePolicy, *put.LifecyclePolicyText)

	repoBody := []byte(`{"repositoryName":"team/app"}`)
	out, err = GetLifecyclePolicy(context.Background(), nc, policyTestAccount, repoBody)
	require.NoError(t, err)
	got, ok := out.(*ecr.GetLifecyclePolicyOutput)
	require.True(t, ok)
	assert.Equal(t, validLifecyclePolicy, *got.LifecyclePolicyText)
	require.NotNil(t, got.LastEvaluatedAt)
	assert.True(t, got.LastEvaluatedAt.Equal(time.Unix(0, 0)))

	w := httptest.NewRecorder()
	WriteJSONResponse(w, got)
	assert.Contains(t, w.Body.String(), `"lastEvaluatedAt":0`)

	out, err = DeleteLifecyclePolicy(context.Background(), nc, policyTestAccount, repoBody)
	require.NoError(t, err)
	del, ok := out.(*ecr.DeleteLifecyclePolicyOutput)
	require.True(t, ok)
	assert.Equal(t, validLifecyclePolicy, *del.LifecyclePolicyText)
	require.NotNil(t, del.LastEvaluatedAt)
	assert.True(t, del.LastEvaluatedAt.Equal(time.Unix(0, 0)))
}

func TestLifecyclePolicy_NotFoundMessagesNameRepository(t *testing.T) {
	nc := newLifecycleTestConn(t)
	seedRepo(t, nc, "team/app")

	_, err := GetLifecyclePolicy(context.Background(), nc, policyTestAccount, []byte(`{"repositoryName":"team/app"}`))
	require.Error(t, err)
	code, message, found := awserrors.ResolveErrorDetail(err)
	require.True(t, found)
	assert.Equal(t, awserrors.ErrorLifecyclePolicyNotFound, code)
	assert.Equal(t, "Lifecycle policy does not exist for the repository with name 'team/app' in the registry with id '"+policyTestAccount+"'", message)

	_, err = GetLifecyclePolicy(context.Background(), nc, policyTestAccount, []byte(`{"repositoryName":"team/ghost"}`))
	require.Error(t, err)
	code, message, found = awserrors.ResolveErrorDetail(err)
	require.True(t, found)
	assert.Equal(t, awserrors.ErrorRepositoryNotFound, code)
	assert.Equal(t, "The repository with name 'team/ghost' does not exist in the registry with id '"+policyTestAccount+"'", message)
}
