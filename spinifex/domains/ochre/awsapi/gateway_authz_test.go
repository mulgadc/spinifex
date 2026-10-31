package awsapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mulgadc/bluebottle/pkg/sigv4"
	"github.com/mulgadc/spinifex/internal/testkit/gwtest"
	awsapi "github.com/mulgadc/spinifex/spinifex/domains/ochre/awsapi"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/credential"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dispatchBedrockFamily drives one Bedrock service through the registered
// dispatcher with no stores behind it. A request that clears the policy gate
// reaches the handler and fails there, which is what proves the gate ran
// ahead of the resource existing.
func dispatchBedrockFamily(t *testing.T, h *gwtest.Harness, service, method, path, body string) error {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	return h.Dispatch(awsapi.NewRegistration(awsapi.Deps{}), service, httptest.NewRecorder(), req)
}

func bedrockScopeARN(resource string) string {
	return "arn:aws:bedrock:" + gwtest.Region + ":" + gwtest.AccountID + ":" + resource
}

// TestBedrockRequest_ScopedDenyFires is the bypass this work closes. An operator
// fences a production guardrail; before the resolver the fence was inert and
// DeleteGuardrail against it was permitted with nothing logged.
func TestBedrockRequest_ScopedDenyFires(t *testing.T) {
	h := gwtest.Scoped(
		gwtest.Statement("Allow", "bedrock:*", "*"),
		gwtest.Statement("Deny", "bedrock:DeleteGuardrail", bedrockScopeARN("guardrail/gr-prod")),
	)

	gwtest.AssertDenied(t, dispatchBedrockFamily(t, h, "bedrock", http.MethodDelete, "/guardrails/gr-prod", ""))
	assertBedrockPermitted(t, dispatchBedrockFamily(t, h, "bedrock", http.MethodDelete, "/guardrails/gr-dev", ""))
}

// TestBedrockRequest_ScopedAllowGrants is the other half: a least-privilege
// policy used to deny everything, so the only working shape was Resource "*".
func TestBedrockRequest_ScopedAllowGrants(t *testing.T) {
	h := gwtest.Scoped(
		gwtest.Statement("Allow", "bedrock:GetProvisionedModelThroughput", bedrockScopeARN("provisioned-model/pm-dev")),
	)

	assertBedrockPermitted(t, dispatchBedrockFamily(t, h, "bedrock",
		http.MethodGet, "/provisioned-model-throughput/pm-dev", ""))
	gwtest.AssertDenied(t, dispatchBedrockFamily(t, h, "bedrock",
		http.MethodGet, "/provisioned-model-throughput/pm-prod", ""))
}

// The foundation model a commitment names arrives in the body, so the body read
// has to happen before the gate for a fence on the model to fire at all.
func TestBedrockRequest_CreateProvisionedThroughputScopesFromBody(t *testing.T) {
	h := gwtest.Scoped(
		gwtest.Statement("Allow", "bedrock:*", "*"),
		gwtest.Statement("Deny", "bedrock:CreateProvisionedModelThroughput",
			"arn:aws:bedrock:*::foundation-model/anthropic.*"),
	)

	gwtest.AssertDenied(t, dispatchBedrockFamily(t, h, "bedrock", http.MethodPost,
		"/provisioned-model-throughput", `{"modelId":"anthropic.claude-v2"}`))
	assertBedrockPermitted(t, dispatchBedrockFamily(t, h, "bedrock", http.MethodPost,
		"/provisioned-model-throughput", `{"modelId":"meta.llama3"}`))
	// An unreadable body authorizes account-wide and stays the handler's fault.
	assertBedrockPermitted(t, dispatchBedrockFamily(t, h, "bedrock", http.MethodPost,
		"/provisioned-model-throughput", "{not json"))
}

// bedrock-runtime names its model in the path, so a fence on a model prefix
// fences inference against it.
func TestBedrockRuntimeRequest_ScopedDenyFires(t *testing.T) {
	h := gwtest.Scoped(
		gwtest.Statement("Allow", "bedrock:*", "*"),
		gwtest.Statement("Deny", "bedrock:InvokeModel", "arn:aws:bedrock:*::foundation-model/anthropic.*"),
	)

	gwtest.AssertDenied(t, dispatchBedrockFamily(t, h, "bedrock-runtime",
		http.MethodPost, "/model/anthropic.claude-v2/invoke", `{}`))
	assertBedrockPermitted(t, dispatchBedrockFamily(t, h, "bedrock-runtime",
		http.MethodPost, "/model/meta.llama3/invoke", `{}`))
}

// bedrock-agent's data-source and ingestion-job actions are evaluated against
// the knowledge base, so a fence on the base covers them.
func TestBedrockAgentRequest_ScopedDenyCoversTheWholeKnowledgeBase(t *testing.T) {
	h := gwtest.Scoped(
		gwtest.Statement("Allow", "bedrock:*", "*"),
		gwtest.Statement("Deny", "bedrock:StartIngestionJob", bedrockScopeARN("knowledge-base/kb-prod")),
	)

	assertBedrockPermitted(t, dispatchBedrockFamily(t, h, "bedrock-agent", http.MethodPut,
		"/knowledgebases/kb-dev/datasources/ds-1/ingestionjobs/", `{}`))
	gwtest.AssertDenied(t, dispatchBedrockFamily(t, h, "bedrock-agent", http.MethodPut,
		"/knowledgebases/kb-prod/datasources/ds-1/ingestionjobs/", `{}`))
}

// RetrieveAndGenerate names its knowledge base in the body, two levels down.
func TestBedrockAgentRuntimeRequest_RetrieveAndGenerateScopesFromBody(t *testing.T) {
	h := gwtest.Scoped(
		gwtest.Statement("Allow", "bedrock:*", "*"),
		gwtest.Statement("Deny", "bedrock:RetrieveAndGenerate", bedrockScopeARN("knowledge-base/kb-prod")),
	)
	body := func(kb string) string {
		return `{"input":{"text":"q"},"retrieveAndGenerateConfiguration":{"type":"KNOWLEDGE_BASE",` +
			`"knowledgeBaseConfiguration":{"knowledgeBaseId":"` + kb + `","modelArn":"meta.llama3"}}}`
	}

	gwtest.AssertDenied(t, dispatchBedrockFamily(t, h, "bedrock-agent-runtime",
		http.MethodPost, "/retrieveAndGenerate", body("kb-prod")))
	assertBedrockPermitted(t, dispatchBedrockFamily(t, h, "bedrock-agent-runtime",
		http.MethodPost, "/retrieveAndGenerate", body("kb-dev")))
}

// A body past the signed path's cap cannot be used to bypass the gate or to
// exhaust memory: it is rejected before either the gate or the handler.
func TestBedrockRequest_OversizedBodyIsRejected(t *testing.T) {
	h := gwtest.Scoped(gwtest.Statement("Allow", "bedrock:*", "*"))
	body := `{"modelId":"` + strings.Repeat("a", sigv4.MaxPayloadLen) + `"}`

	err := dispatchBedrockFamily(t, h, "bedrock", http.MethodPost,
		"/provisioned-model-throughput", body)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorRequestEntityTooLarge, err.Error())
}

// The property the per-service rollout rests on: passing a real ARN where "*"
// was passed before cannot withdraw access a working policy already grants.
func TestBedrockFamily_AccountWideGrantStillPermitsEveryAction(t *testing.T) {
	h := gwtest.Scoped(gwtest.Statement("Allow", "bedrock:*", "*"))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	caller := credential.Principal{
		AccountID:     gwtest.AccountID,
		Identity:      "alice",
		PrincipalType: stsdomain.PrincipalTypeUser,
		CallerARN:     "arn:aws:iam::" + gwtest.AccountID + ":user/alice",
	}
	body := []byte(`{"modelId":"meta.llama3","retrieveAndGenerateConfiguration":` +
		`{"knowledgeBaseConfiguration":{"knowledgeBaseId":"kb-1"}}}`)

	for _, service := range []string{"bedrock", "bedrock-runtime", "bedrock-agent", "bedrock-agent-runtime"} {
		for _, action := range awsapi.ScopedActions(service) {
			t.Run(service+"/"+action, func(t *testing.T) {
				resources, err := awsapi.ResourceARNs(service, action, gwtest.Region, gwtest.AccountID,
					[]string{"id-1", "id-2", "id-3"}, body)
				require.NoError(t, err)
				assert.NoError(t, pipeline.Authorize(h.Policy, req, caller, true, service, action, resources, nil))
			})
		}
	}
}

// The account-ID read moved above the gate, which used to reject a missing
// account itself. InternalError is the code the caller has always seen.
func TestBedrockFamily_MissingAccountIDReturnsInternalError(t *testing.T) {
	h := gwtest.Scoped(gwtest.Statement("Allow", "bedrock:*", "*"))
	h.AccountID = ""
	dispatchers := map[string]struct {
		method string
		path   string
	}{
		"bedrock":               {http.MethodGet, "/foundation-models"},
		"bedrock-runtime":       {http.MethodPost, "/model/m/invoke"},
		"bedrock-agent":         {http.MethodPost, "/knowledgebases/"},
		"bedrock-agent-runtime": {http.MethodPost, "/retrieveAndGenerate"},
	}

	for service, d := range dispatchers {
		t.Run(service, func(t *testing.T) {
			req := httptest.NewRequest(d.method, d.path, strings.NewReader("{}"))
			err := h.Dispatch(awsapi.NewRegistration(awsapi.Deps{}), service, httptest.NewRecorder(), req)
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorInternalError, err.Error())
		})
	}
}

// assertBedrockPermitted asserts the request cleared the policy gate. Past it
// the handler has no store behind it, so any other outcome is acceptable.
func assertBedrockPermitted(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	code, _ := awserrors.ResolveErrorCode(err)
	assert.NotEqual(t, awserrors.ErrorAccessDenied, code, "policy gate denied: %v", err)
}
