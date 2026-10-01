package awsapi

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
	handlers_ecr "github.com/mulgadc/spinifex/spinifex/domains/ecr"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const describeRepositoriesTestAccount = "000000000000"

func newDescribeRepositoriesTestConn(t *testing.T) *nats.Conn {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	svc := handlers_ecr.NewKVMetaService(js)
	serveDescribeRepositoriesMeta(t, nc, handlers_ecr.SubjectRepoCreate, svc.RepoCreate)
	serveDescribeRepositoriesMeta(t, nc, handlers_ecr.SubjectRepoDescribe, svc.RepoDescribe)
	serveDescribeRepositoriesMeta(t, nc, handlers_ecr.SubjectRepoList, svc.RepoList)
	return nc
}

func serveDescribeRepositoriesMeta[I any, O any](t *testing.T, nc *nats.Conn, subject string, fn func(context.Context, *I, string) (*O, error)) {
	t.Helper()
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		accountID := utils.AccountIDFromMsg(msg)
		in := new(I)
		if errResp := utils.UnmarshalJsonPayload(in, msg.Data); errResp != nil {
			_ = msg.Respond(errResp)
			return
		}
		out, err := fn(context.Background(), in, accountID)
		if err != nil {
			_ = msg.Respond(utils.GenerateErrorPayload("ServerInternal"))
			return
		}
		data, _ := json.Marshal(out)
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

func seedDescribeRepository(t *testing.T, nc *nats.Conn, name string) {
	t.Helper()
	store := handlers_ecr.NewNATSMetaStore(nc)
	require.NoError(t, store.PutRepo(context.Background(), describeRepositoriesTestAccount, handlers_ecr.RepoMeta{
		Name: name, CreatedAt: time.Now(),
	}))
}

func describeRepositoriesEndpoint() RepositoryEndpoint {
	return RepositoryEndpoint{Region: "ap-southeast-2", ServicesDomain: "spinifex.test"}
}

func TestDescribeRepositories_ListsAccountScoped(t *testing.T) {
	nc := newDescribeRepositoriesTestConn(t)
	seedDescribeRepository(t, nc, "team/app")
	seedDescribeRepository(t, nc, "team/web")

	out, err := DescribeRepositories(context.Background(), nc, describeRepositoriesEndpoint(), describeRepositoriesTestAccount, []byte(`{}`))
	require.NoError(t, err)
	require.Len(t, out.Repositories, 2)

	byName := map[string]int{}
	for i, repository := range out.Repositories {
		byName[*repository.RepositoryName] = i
	}
	app := out.Repositories[byName["team/app"]]
	assert.Equal(t, describeRepositoriesTestAccount, *app.RegistryId)
	assert.Equal(t, "arn:aws:ecr:ap-southeast-2:"+describeRepositoriesTestAccount+":repository/team/app", *app.RepositoryArn)
	assert.Equal(t, describeRepositoriesTestAccount+".dkr.ecr.ap-southeast-2.spinifex.test/team/app", *app.RepositoryUri)
}

func TestDescribeRepositories_ValidatesRequestAndRepository(t *testing.T) {
	nc := newDescribeRepositoriesTestConn(t)
	seedDescribeRepository(t, nc, "team/app")

	cases := []struct {
		name string
		body string
		code string
	}{
		{"malformed body", `{`, awserrors.ErrorInvalidParameterValue},
		{"cross-account registry", `{"registryId":"999999999999"}`, awserrors.ErrorAccessDenied},
		{"missing named repository", `{"repositoryNames":["team/ghost"]}`, awserrors.ErrorRepositoryNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DescribeRepositories(context.Background(), nc, describeRepositoriesEndpoint(), describeRepositoriesTestAccount, []byte(tc.body))
			require.Error(t, err)
			assert.Equal(t, tc.code, awserrors.ValidErrorCodeFromError(err))
		})
	}
}
