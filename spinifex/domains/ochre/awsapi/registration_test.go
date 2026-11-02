//test:in-package — exercises the unexported dispatch split and accessors.

package awsapi

import (
	"context"
	"errors"
	"net/http/httptest"
	"slices"
	"testing"

	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRegistration_RegistersEveryScopeName(t *testing.T) {
	b := dispatch.NewBuilder()
	require.NoError(t, b.Register(NewRegistration(Deps{})))
	reg := b.Build()
	for _, name := range []string{"bedrock", "bedrock-runtime", "bedrock-agent", "bedrock-agent-runtime"} {
		entry, ok := reg.Lookup(name)
		require.True(t, ok, name)
		assert.Equal(t, ServiceName, entry.Service(), name)
	}
	_, ok := reg.Lookup("bedrock-foo")
	assert.False(t, ok)
}

func TestOperationInventory_ListsEverySubserviceActionOnce(t *testing.T) {
	inv := OperationInventory()
	assert.Empty(t, inv.Stubbed)
	assert.True(t, slices.IsSorted(inv.Registered))
	for _, want := range []string{"ListFoundationModels", "Converse", "CreateKnowledgeBase", "Retrieve"} {
		assert.Contains(t, inv.Registered, want)
	}
}

func TestResolveAction_LabelsByPath(t *testing.T) {
	cases := []struct {
		method, path, want string
	}{
		{"GET", "/foundation-models", "ListFoundationModels"},
		{"POST", "/model/m/converse", "Converse"},
		{"POST", "/guardrail/g/version/1/apply", "ApplyGuardrail"},
		{"POST", "/knowledgebases/", "ListKnowledgeBases"},
		{"POST", "/knowledgebases/kb/retrieve", "Retrieve"},
		{"POST", "/retrieveAndGenerate", "RetrieveAndGenerate"},
		{"GET", "/nope", ""},
		{"GET", "/knowledgebases/kb/retrieve", ""},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.path, nil)
		assert.Equal(t, c.want, resolveAction(r), c.method+" "+c.path)
	}
}

func TestDispatch_SubserviceScopeSelectsDispatcherOutright(t *testing.T) {
	g := &testGateway{}
	cases := []struct {
		service string
		path    string
		method  string
		wantErr string
	}{
		{ServiceName, "/nope", "GET", awserrors.ErrorInvalidAction},
		{serviceRuntime, "/guardrails", "GET", awserrors.ErrorInvalidAction},
		{serviceAgent, "/guardrails", "GET", awserrors.ErrorInvalidAction},
		{serviceAgentRuntime, "/guardrails", "GET", awserrors.ErrorInvalidAction},
		{serviceAgent, "/model/m/converse", "POST", awserrors.ErrorInvalidAction},
		{"", "/model/m/nope", "POST", awserrors.ErrorInvalidAction},
	}
	for _, c := range cases {
		err := g.serve(c.service, httptest.NewRecorder(), requestForAccount("111122223333", c.method, c.path, ""))
		require.Error(t, err, c.service+" "+c.path)
		assert.Equal(t, c.wantErr, err.Error(), c.service+" "+c.path)
	}
}

type countingQuota struct {
	rpm, tokens int
	rpmErr      error
}

func (q *countingQuota) CheckBedrockRPM(string) error { q.rpm++; return q.rpmErr }
func (q *countingQuota) CheckBedrockTokens(context.Context, string) error {
	q.tokens++
	return nil
}

func TestDispatch_RuntimeMeteringRunsBeforeTheBodyIsRead(t *testing.T) {
	q := &countingQuota{rpmErr: assertErr(awserrors.ErrorThrottlingException)}
	g := &testGateway{Deps: Deps{Quota: q}}
	err := g.BedrockRuntime_Request(httptest.NewRecorder(), requestForAccount("111122223333", "POST", "/model/m/converse", "{"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorThrottlingException, err.Error())
	assert.Equal(t, 1, q.rpm)
	assert.Zero(t, q.tokens)
}

func assertErr(code string) error { return errors.New(code) }

func TestDeps_NilCapabilitiesFallBack(t *testing.T) {
	var d Deps
	assert.NoError(t, d.checkRPM("a"))
	assert.NoError(t, d.checkTokens(context.Background(), "a"))
	assert.NotNil(t, d.resolver())
	assert.NotNil(t, d.recorder())
	assert.NotNil(t, d.loggingStore())
	assert.NotNil(t, d.provisionedStore("us-east-1"))
	assert.NotNil(t, d.guardrailStore("us-east-1"))
	_, ok, err := d.endpointResolver().Endpoint(context.Background(), "m")
	require.NoError(t, err)
	assert.False(t, ok)
	granted, err := d.accessResolver().Granted(context.Background(), "111122223333", "m")
	require.NoError(t, err)
	assert.False(t, granted, "no grant store denies every model")
}

func TestResolveSubservice_SplitsTheSharedSigningNameByPath(t *testing.T) {
	cases := []struct{ path, want string }{
		{"/model/meta.llama3-70b-instruct-v1:0/converse", serviceRuntime},
		{"/model/meta.llama3-70b-instruct-v1:0/converse-stream", serviceRuntime},
		{"/guardrail/gr-abc123/version/1/apply", serviceRuntime},
		{"/foundation-models", ServiceName},
		{"/guardrails", ServiceName},
		{"/guardrails/gr-abc123", ServiceName},
		{"/knowledgebases/", serviceAgent},
		{"/knowledgebases/kb-123", serviceAgent},
		{"/knowledgebases/kb-123/datasources/", serviceAgent},
		{"/knowledgebases/kb-123/retrieve", serviceAgentRuntime},
		{"/retrieveAndGenerate", serviceAgentRuntime},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, resolveSubservice(c.path), c.path)
	}
}
