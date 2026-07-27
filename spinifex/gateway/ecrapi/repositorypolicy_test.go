package gateway_ecrapi

import (
	"context"

	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ecr "github.com/mulgadc/spinifex/spinifex/handlers/ecr"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const policyTestAccount = "000000000000"

// serveMeta wires a MetaService method to a NATS subject, mirroring the daemon.
func serveMeta[I any, O any](t *testing.T, nc *nats.Conn, subject string, fn func(context.Context, *I, string) (*O, error)) {
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

// newPolicyTestConn starts an embedded JetStream server with the repo + policy
// metadata subjects served, returning the gateway NATS connection.
func newPolicyTestConn(t *testing.T) *nats.Conn {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	svc := handlers_ecr.NewKVMetaService(js)
	serveMeta(t, nc, handlers_ecr.SubjectRepoCreate, svc.RepoCreate)
	serveMeta(t, nc, handlers_ecr.SubjectRepoDescribe, svc.RepoDescribe)
	serveMeta(t, nc, handlers_ecr.SubjectPolicyPut, svc.PolicyPut)
	serveMeta(t, nc, handlers_ecr.SubjectPolicyGet, svc.PolicyGet)
	serveMeta(t, nc, handlers_ecr.SubjectPolicyDelete, svc.PolicyDelete)
	return nc
}

func seedRepo(t *testing.T, nc *nats.Conn, repo string) {
	t.Helper()
	store := handlers_ecr.NewNATSMetaStore(nc)
	require.NoError(t, store.PutRepo(context.Background(), policyTestAccount, handlers_ecr.RepoMeta{Name: repo, CreatedAt: time.Now()}))
}

func TestRepositoryPolicy_Lifecycle(t *testing.T) {
	nc := newPolicyTestConn(t)
	seedRepo(t, nc, "team/app")
	const policy = `{"Version":"2012-10-17","Statement":[]}`
	body := []byte(`{"repositoryName":"team/app","policyText":` + strconvQuote(policy) + `}`)

	out, err := SetRepositoryPolicy(context.Background(), nc, policyTestAccount, body)
	require.NoError(t, err)
	set, ok := out.(*ecr.SetRepositoryPolicyOutput)
	require.True(t, ok)
	assert.Equal(t, policy, *set.PolicyText)
	assert.Equal(t, "team/app", *set.RepositoryName)
	assert.Equal(t, policyTestAccount, *set.RegistryId)

	out, err = GetRepositoryPolicy(context.Background(), nc, policyTestAccount, []byte(`{"repositoryName":"team/app"}`))
	require.NoError(t, err)
	got, ok := out.(*ecr.GetRepositoryPolicyOutput)
	require.True(t, ok)
	assert.Equal(t, policy, *got.PolicyText)

	out, err = DeleteRepositoryPolicy(context.Background(), nc, policyTestAccount, []byte(`{"repositoryName":"team/app"}`))
	require.NoError(t, err)
	del, ok := out.(*ecr.DeleteRepositoryPolicyOutput)
	require.True(t, ok)
	assert.Equal(t, policy, *del.PolicyText)

	// Policy gone after delete.
	_, err = GetRepositoryPolicy(context.Background(), nc, policyTestAccount, []byte(`{"repositoryName":"team/app"}`))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorRepositoryPolicyNotFound, err.Error())
}

func TestRepositoryPolicy_Errors(t *testing.T) {
	nc := newPolicyTestConn(t)
	seedRepo(t, nc, "team/app")

	cases := []struct {
		name   string
		fn     func(context.Context, *nats.Conn, string, []byte) (any, error)
		body   string
		expect string
	}{
		{"set missing repo", SetRepositoryPolicy, `{"repositoryName":"team/ghost","policyText":"{}"}`, awserrors.ErrorRepositoryNotFound},
		{"set invalid json policy", SetRepositoryPolicy, `{"repositoryName":"team/app","policyText":"not-json"}`, awserrors.ErrorInvalidParameterValue},
		{"set empty name", SetRepositoryPolicy, `{"policyText":"{}"}`, awserrors.ErrorInvalidParameterValue},
		{"set cross-account", SetRepositoryPolicy, `{"repositoryName":"team/app","registryId":"999999999999","policyText":"{}"}`, awserrors.ErrorAccessDenied},
		{"get no policy", GetRepositoryPolicy, `{"repositoryName":"team/app"}`, awserrors.ErrorRepositoryPolicyNotFound},
		{"delete no policy", DeleteRepositoryPolicy, `{"repositoryName":"team/app"}`, awserrors.ErrorRepositoryPolicyNotFound},
		{"get cross-account", GetRepositoryPolicy, `{"repositoryName":"team/app","registryId":"999999999999"}`, awserrors.ErrorAccessDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.fn(context.Background(), nc, policyTestAccount, []byte(tc.body))
			require.Error(t, err)
			assert.Equal(t, tc.expect, err.Error())
		})
	}
}

// strconvQuote JSON-quotes a string for inline test bodies.
func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
