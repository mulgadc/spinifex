//test:in-package — drives the four Bedrock-family dispatchers through the
// gateway's unexported test helpers (withTestIdentity, policyMockIAMService)
// and auth context keys.

package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mulgadc/bluebottle/pkg/sigv4"
	ochreawsapi "github.com/mulgadc/spinifex/spinifex/domains/ochre/awsapi"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dispatchBedrockFamily drives one Bedrock service through the registered
// dispatcher with no stores behind it. A request that clears the policy gate
// reaches the handler and fails there, which is what proves the gate ran
// ahead of the resource existing.
func dispatchBedrockFamily(t *testing.T, gw *GatewayConfig, service, method, path, body string) error {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	ctx := context.WithValue(req.Context(), ctxService, service)
	req = withTestIdentity(req.WithContext(withPrincipalField(ctx, fieldAccountID, authzAccountID)))
	return serveBedrock(gw, service, httptest.NewRecorder(), req)
}

func serveBedrock(gw *GatewayConfig, service string, w http.ResponseWriter, req *http.Request) error {
	if gw.Services == nil {
		b := dispatch.NewBuilder()
		if err := b.Register(ochreawsapi.NewRegistration(ochreawsapi.Deps{})); err != nil {
			panic(err)
		}
		gw.Services = b.Build()
	}
	entry, ok := gw.registered(service)
	if !ok {
		panic(service + " is not registered on this gateway")
	}
	return gw.dispatchRegistered(entry, w, req)
}

func bedrockScopeARN(resource string) string {
	return "arn:aws:bedrock:" + authzRegion + ":" + authzAccountID + ":" + resource
}

// TestBedrockRequest_ScopedDenyFires is the bypass this work closes. An operator
// fences a production guardrail; before the resolver the fence was inert and
// DeleteGuardrail against it was permitted with nothing logged.
func TestBedrockRequest_ScopedDenyFires(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "bedrock:*", "*"),
		statement("Deny", "bedrock:DeleteGuardrail", bedrockScopeARN("guardrail/gr-prod")),
	)

	assertDenied(t, dispatchBedrockFamily(t, gw, "bedrock", http.MethodDelete, "/guardrails/gr-prod", ""))
	assertBedrockPermitted(t, dispatchBedrockFamily(t, gw, "bedrock", http.MethodDelete, "/guardrails/gr-dev", ""))
}

// TestBedrockRequest_ScopedAllowGrants is the other half: a least-privilege
// policy used to deny everything, so the only working shape was Resource "*".
func TestBedrockRequest_ScopedAllowGrants(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "bedrock:GetProvisionedModelThroughput", bedrockScopeARN("provisioned-model/pm-dev")),
	)

	assertBedrockPermitted(t, dispatchBedrockFamily(t, gw, "bedrock",
		http.MethodGet, "/provisioned-model-throughput/pm-dev", ""))
	assertDenied(t, dispatchBedrockFamily(t, gw, "bedrock",
		http.MethodGet, "/provisioned-model-throughput/pm-prod", ""))
}

// The foundation model a commitment names arrives in the body, so the body read
// has to happen before the gate for a fence on the model to fire at all.
func TestBedrockRequest_CreateProvisionedThroughputScopesFromBody(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "bedrock:*", "*"),
		statement("Deny", "bedrock:CreateProvisionedModelThroughput",
			"arn:aws:bedrock:*::foundation-model/anthropic.*"),
	)

	assertDenied(t, dispatchBedrockFamily(t, gw, "bedrock", http.MethodPost,
		"/provisioned-model-throughput", `{"modelId":"anthropic.claude-v2"}`))
	assertBedrockPermitted(t, dispatchBedrockFamily(t, gw, "bedrock", http.MethodPost,
		"/provisioned-model-throughput", `{"modelId":"meta.llama3"}`))
	// An unreadable body authorizes account-wide and stays the handler's fault.
	assertBedrockPermitted(t, dispatchBedrockFamily(t, gw, "bedrock", http.MethodPost,
		"/provisioned-model-throughput", "{not json"))
}

// bedrock-runtime names its model in the path, so a fence on a model prefix
// fences inference against it.
func TestBedrockRuntimeRequest_ScopedDenyFires(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "bedrock:*", "*"),
		statement("Deny", "bedrock:InvokeModel", "arn:aws:bedrock:*::foundation-model/anthropic.*"),
	)

	assertDenied(t, dispatchBedrockFamily(t, gw, "bedrock-runtime",
		http.MethodPost, "/model/anthropic.claude-v2/invoke", `{}`))
	assertBedrockPermitted(t, dispatchBedrockFamily(t, gw, "bedrock-runtime",
		http.MethodPost, "/model/meta.llama3/invoke", `{}`))
}

// bedrock-agent's data-source and ingestion-job actions are evaluated against
// the knowledge base, so a fence on the base covers them.
func TestBedrockAgentRequest_ScopedDenyCoversTheWholeKnowledgeBase(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "bedrock:*", "*"),
		statement("Deny", "bedrock:StartIngestionJob", bedrockScopeARN("knowledge-base/kb-prod")),
	)

	assertBedrockPermitted(t, dispatchBedrockFamily(t, gw, "bedrock-agent", http.MethodPut,
		"/knowledgebases/kb-dev/datasources/ds-1/ingestionjobs/", `{}`))
	assertDenied(t, dispatchBedrockFamily(t, gw, "bedrock-agent", http.MethodPut,
		"/knowledgebases/kb-prod/datasources/ds-1/ingestionjobs/", `{}`))
}

// RetrieveAndGenerate names its knowledge base in the body, two levels down.
func TestBedrockAgentRuntimeRequest_RetrieveAndGenerateScopesFromBody(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "bedrock:*", "*"),
		statement("Deny", "bedrock:RetrieveAndGenerate", bedrockScopeARN("knowledge-base/kb-prod")),
	)
	body := func(kb string) string {
		return `{"input":{"text":"q"},"retrieveAndGenerateConfiguration":{"type":"KNOWLEDGE_BASE",` +
			`"knowledgeBaseConfiguration":{"knowledgeBaseId":"` + kb + `","modelArn":"meta.llama3"}}}`
	}

	assertDenied(t, dispatchBedrockFamily(t, gw, "bedrock-agent-runtime",
		http.MethodPost, "/retrieveAndGenerate", body("kb-prod")))
	assertBedrockPermitted(t, dispatchBedrockFamily(t, gw, "bedrock-agent-runtime",
		http.MethodPost, "/retrieveAndGenerate", body("kb-dev")))
}

// A body past the signed path's cap cannot be used to bypass the gate or to
// exhaust memory: it is rejected before either the gate or the handler.
func TestBedrockRequest_OversizedBodyIsRejected(t *testing.T) {
	gw := scopedPolicyGateway(statement("Allow", "bedrock:*", "*"))
	body := `{"modelId":"` + strings.Repeat("a", sigv4.MaxPayloadLen) + `"}`

	err := dispatchBedrockFamily(t, gw, "bedrock", http.MethodPost,
		"/provisioned-model-throughput", body)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorRequestEntityTooLarge, err.Error())
}

// The property the per-service rollout rests on: passing a real ARN where "*"
// was passed before cannot withdraw access a working policy already grants.
func TestBedrockFamily_AccountWideGrantStillPermitsEveryAction(t *testing.T) {
	gw := scopedPolicyGateway(statement("Allow", "bedrock:*", "*"))
	req := withTestIdentity(httptest.NewRequest(http.MethodGet, "/", nil).
		WithContext(withPrincipalField(context.Background(), fieldAccountID, authzAccountID)))
	body := []byte(`{"modelId":"meta.llama3","retrieveAndGenerateConfiguration":` +
		`{"knowledgeBaseConfiguration":{"knowledgeBaseId":"kb-1"}}}`)

	for _, service := range []string{"bedrock", "bedrock-runtime", "bedrock-agent", "bedrock-agent-runtime"} {
		for _, action := range ochreawsapi.ScopedActions(service) {
			t.Run(service+"/"+action, func(t *testing.T) {
				resources, err := ochreawsapi.ResourceARNs(service, action, authzRegion, authzAccountID,
					[]string{"id-1", "id-2", "id-3"}, body)
				require.NoError(t, err)
				assert.NoError(t, gw.checkPolicyResources(req, service, action, resources))
			})
		}
	}
}

// The account-ID read moved above the gate, which used to reject a missing
// account itself. InternalError is the code the caller has always seen.
func TestBedrockFamily_MissingAccountIDReturnsInternalError(t *testing.T) {
	gw := scopedPolicyGateway(statement("Allow", "bedrock:*", "*"))
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
			req = withTestIdentity(req.WithContext(context.WithValue(req.Context(), ctxService, service)))
			err := serveBedrock(gw, service, httptest.NewRecorder(), req)
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
