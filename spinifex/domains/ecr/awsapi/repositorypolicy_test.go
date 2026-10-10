package awsapi

import (
	"context"
	"github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"

	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/mulgadc/spinifex/internal/testkit"
	ecrdomain "github.com/mulgadc/spinifex/spinifex/domains/ecr"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const policyTestAccount = "000000000000"

// serveMeta wires a MetaService method to a NATS subject, mirroring the daemon.
func serveMeta[I any, O any](t *testing.T, nc *nats.Conn, subject string, fn func(context.Context, *I, string) (*O, error)) {
	t.Helper()
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		accountID := natsmsg.AccountIDFromMsg(msg)
		in := new(I)
		if errResp := awserrors.UnmarshalJsonPayload(in, msg.Data); errResp != nil {
			_ = msg.Respond(errResp)
			return
		}
		out, err := fn(context.Background(), in, accountID)
		if err != nil {
			_ = msg.Respond(awserrors.GenerateErrorPayload("ServerInternal"))
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
	svc := ecrdomain.NewKVMetaService(js)
	serveMeta(t, nc, ecrdomain.SubjectRepoCreate, svc.RepoCreate)
	serveMeta(t, nc, ecrdomain.SubjectRepoDescribe, svc.RepoDescribe)
	serveMeta(t, nc, ecrdomain.SubjectPolicyPut, svc.PolicyPut)
	serveMeta(t, nc, ecrdomain.SubjectPolicyGet, svc.PolicyGet)
	serveMeta(t, nc, ecrdomain.SubjectPolicyDelete, svc.PolicyDelete)
	return nc
}

func seedRepo(t *testing.T, nc *nats.Conn, repo string) {
	t.Helper()
	store := ecrdomain.NewNATSMetaStore(nc)
	require.NoError(t, store.PutRepo(context.Background(), policyTestAccount, ecrdomain.RepoMeta{Name: repo, CreatedAt: time.Now()}))
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

	// Policy gone after delete; AWS names the repository and registry.
	_, err = GetRepositoryPolicy(context.Background(), nc, policyTestAccount, []byte(`{"repositoryName":"team/app"}`))
	require.Error(t, err)
	code, message, found := awserrors.ResolveErrorDetail(err)
	require.True(t, found)
	assert.Equal(t, awserrors.ErrorRepositoryPolicyNotFound, code)
	assert.Equal(t, "Repository policy does not exist for the repository with name 'team/app' in the registry with id '"+policyTestAccount+"'", message)
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
		{"set invalid json policy", SetRepositoryPolicy, `{"repositoryName":"team/app","policyText":"not-json"}`, awserrors.ErrorECRInvalidParameter},
		{"set empty name", SetRepositoryPolicy, `{"policyText":"{}"}`, awserrors.ErrorECRInvalidParameter},
		{"set cross-account", SetRepositoryPolicy, `{"repositoryName":"team/app","registryId":"999999999999","policyText":"{}"}`, awserrors.ErrorAccessDenied},
		{"get no policy", GetRepositoryPolicy, `{"repositoryName":"team/app"}`, awserrors.ErrorRepositoryPolicyNotFound},
		{"delete no policy", DeleteRepositoryPolicy, `{"repositoryName":"team/app"}`, awserrors.ErrorRepositoryPolicyNotFound},
		{"get cross-account", GetRepositoryPolicy, `{"repositoryName":"team/app","registryId":"999999999999"}`, awserrors.ErrorAccessDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.fn(context.Background(), nc, policyTestAccount, []byte(tc.body))
			require.Error(t, err)
			assert.Equal(t, tc.expect, awserrors.ValidErrorCodeFromError(err))
		})
	}
}

// AWS names the member PolicyText, capitalised, when SetRepositoryPolicy omits it.
func TestSetRepositoryPolicy_MissingPolicyTextMessage(t *testing.T) {
	nc := newPolicyTestConn(t)
	seedRepo(t, nc, "team/app")
	_, err := SetRepositoryPolicy(context.Background(), nc, policyTestAccount, []byte(`{"repositoryName":"team/app"}`))
	require.Error(t, err)
	code, message, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorECRInvalidParameter, code)
	assert.Equal(t, "Invalid parameter at 'PolicyText' failed to satisfy constraint: 'Cannot be null'", message)
}

// strconvQuote JSON-quotes a string for inline test bodies.
func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
