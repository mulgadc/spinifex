package gateway_bedrock_test

import (
	"testing"

	gateway_bedrock "github.com/mulgadc/spinifex/spinifex/gateway/bedrock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	authzRegion    = "ap-southeast-2"
	authzAccountID = "123456789012"
)

func bedrockARN(resource string) string {
	return "arn:aws:bedrock:" + authzRegion + ":" + authzAccountID + ":" + resource
}

func resolveScope(t *testing.T, service, action string, params []string, body string) []string {
	t.Helper()
	resources, err := gateway_bedrock.ResourceARNs(service, action, authzRegion, authzAccountID, params, []byte(body))
	require.NoError(t, err)
	return resources
}

// AWS spells a foundation-model ARN with an empty account segment, and a policy
// written against AWS must match here.
func TestResourceARNs_FoundationModelARNHasNoAccount(t *testing.T) {
	assert.Equal(t, []string{"arn:aws:bedrock:" + authzRegion + "::foundation-model/anthropic.claude-v2"},
		resolveScope(t, "bedrock", "GetFoundationModel", []string{"anthropic.claude-v2"}, ""))
	assert.Equal(t, []string{"arn:aws:bedrock:" + authzRegion + "::foundation-model/meta.llama3"},
		resolveScope(t, "bedrock-runtime", "Converse", []string{"meta.llama3"}, ""))
}

// The inference path accepts a provisioned-throughput ARN in place of a model
// id, and the gate names what the caller addressed.
func TestResourceARNs_InferenceAgainstAProvisionedARNNamesTheCommitment(t *testing.T) {
	pt := gateway_bedrock.FormatProvisionedModelARN(authzRegion, authzAccountID, "pm-1")
	assert.Equal(t, []string{bedrockARN("provisioned-model/pm-1")},
		resolveScope(t, "bedrock-runtime", "InvokeModel", []string{pt}, ""))
}

// A commitment or guardrail is addressable by bare id or by ARN; both resolve
// to the same resource, and a foreign account resolves account-wide so the
// handler's own validation fault survives.
func TestResourceARNs_IDOrARNSpellingsAgree(t *testing.T) {
	byID := resolveScope(t, "bedrock", "GetGuardrail", []string{"gr-1"}, "")
	byARN := resolveScope(t, "bedrock", "GetGuardrail",
		[]string{gateway_bedrock.FormatGuardrailARN(authzRegion, authzAccountID, "gr-1")}, "")
	assert.Equal(t, []string{bedrockARN("guardrail/gr-1")}, byID)
	assert.Equal(t, byID, byARN)

	foreign := "arn:aws:bedrock:" + authzRegion + ":999999999999:provisioned-model/pm-1"
	assert.Equal(t, []string{"*"},
		resolveScope(t, "bedrock", "DeleteProvisionedModelThroughput", []string{foreign}, ""))
}

// A create evaluates the commitment it is about to create and the foundation
// model it commits, matching AWS.
func TestResourceARNs_CreateProvisionedThroughputNamesBoth(t *testing.T) {
	assert.Equal(t,
		[]string{
			bedrockARN("provisioned-model/*"),
			"arn:aws:bedrock:" + authzRegion + "::foundation-model/meta.llama3",
		},
		resolveScope(t, "bedrock", "CreateProvisionedModelThroughput", nil, `{"modelId":"meta.llama3"}`))
}

func TestResourceARNs_CreatesResolveTheResourceType(t *testing.T) {
	assert.Equal(t, []string{bedrockARN("guardrail/*")},
		resolveScope(t, "bedrock", "CreateGuardrail", nil, `{"name":"gr"}`))
	assert.Equal(t, []string{bedrockARN("knowledge-base/*")},
		resolveScope(t, "bedrock-agent", "CreateKnowledgeBase", nil, `{"name":"kb"}`))
}

// Data-source and ingestion-job actions are evaluated against the knowledge
// base, so both path ids collapse to one resource.
func TestResourceARNs_AgentActionsCollapseToTheKnowledgeBase(t *testing.T) {
	for _, action := range []string{"GetDataSource", "StartIngestionJob", "GetIngestionJob"} {
		assert.Equal(t, []string{bedrockARN("knowledge-base/kb-1")},
			resolveScope(t, "bedrock-agent", action, []string{"kb-1", "ds-1", "job-1"}, ""),
			"action %q", action)
	}
}

// RetrieveAndGenerate names its knowledge base two levels down in the body,
// which is why the body is read ahead of the gate.
func TestResourceARNs_RetrieveAndGenerateScopesFromTheBody(t *testing.T) {
	body := `{"retrieveAndGenerateConfiguration":{"type":"KNOWLEDGE_BASE","knowledgeBaseConfiguration":{"knowledgeBaseId":"kb-1"}}}`
	assert.Equal(t, []string{bedrockARN("knowledge-base/kb-1")},
		resolveScope(t, "bedrock-agent-runtime", "RetrieveAndGenerate", nil, body))
	assert.Equal(t, []string{bedrockARN("knowledge-base/kb-1")},
		resolveScope(t, "bedrock-agent-runtime", "Retrieve", []string{"kb-1"}, ""))
}

// A body the gate cannot parse, or an absent identifier, authorizes
// account-wide: it is the handler that rejects a malformed request.
func TestResourceARNs_UnparseableBodyAuthorizesAccountWide(t *testing.T) {
	assert.Equal(t, []string{"*"},
		resolveScope(t, "bedrock-agent-runtime", "RetrieveAndGenerate", nil, "{not json"))
	// The commitment it is about to create still resolves; only the foundation
	// model the unreadable body would have named drops out.
	assert.Equal(t, []string{bedrockARN("provisioned-model/*")},
		resolveScope(t, "bedrock", "CreateProvisionedModelThroughput", nil, "{not json"))
	assert.Equal(t, []string{"*"}, resolveScope(t, "bedrock", "GetGuardrail", nil, ""))
}

// The account-level surface is what AWS evaluates against "*".
func TestResourceARNs_AccountLevelActionsStayAccountWide(t *testing.T) {
	for _, action := range []string{
		"ListFoundationModels", "ListGuardrails", "ListProvisionedModelThroughputs",
		"PutModelInvocationLoggingConfiguration", "GetModelInvocationLoggingConfiguration",
		"DeleteModelInvocationLoggingConfiguration",
	} {
		assert.Equal(t, []string{"*"}, resolveScope(t, "bedrock", action, []string{"x"}, `{"modelId":"m"}`),
			"action %q", action)
	}
	assert.Equal(t, []string{"*"}, resolveScope(t, "bedrock-agent", "ListKnowledgeBases", nil, ""))
}

// An action that is served but has no entry, or a service with no table at all,
// fails closed rather than defaulting to an account-wide grant.
func TestResourceARNs_UnknownActionOrServiceIsRejected(t *testing.T) {
	_, err := gateway_bedrock.ResourceARNs("bedrock", "MadeUpAction", authzRegion, authzAccountID, nil, nil)
	require.Error(t, err)
	_, err = gateway_bedrock.ResourceARNs("bedrock-madeup", "GetGuardrail", authzRegion, authzAccountID, nil, nil)
	require.Error(t, err)
}
