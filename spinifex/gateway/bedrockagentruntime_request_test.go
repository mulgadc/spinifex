// In-package: exercises BedrockAgentRuntime_Request's route table and
// dispatcher end-to-end over real HTTP requests, mirroring
// bedrock_request_test.go/bedrockagent_request_test.go's harnesses for the
// sibling bedrock/bedrock-agent services.
//
//test:in-package
package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/service/bedrockagentruntime"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ochrevector "github.com/mulgadc/spinifex/spinifex/handlers/ochrevector"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newBedrockAgentRuntimeRequestGateway builds a GatewayConfig wired the same
// way as newBedrockAgentRequestGateway (real JetStream-backed KB/DataSource
// stores, allow-everything IAMService) plus newBedrockRequestGateway's vLLM
// routing (BedrockEndpoints/BedrockAccess), since RetrieveAndGenerate's
// generation step reaches gateway_bedrock.Converse in-process.
func newBedrockAgentRuntimeRequestGateway(t *testing.T, vector handlers_ochrevector.VectorService, vllmURL string) *GatewayConfig {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	return &GatewayConfig{
		IAMService:              allowAllIAMService(),
		BedrockAgentKB:          handlers_ochrevector.NewKBStore(js),
		BedrockAgentDataSources: handlers_ochrevector.NewDataSourceStore(js),
		BedrockAgentVector:      vector,
		BedrockEndpoints: map[string]string{
			bedrockTestLlamaModelID: vllmURL,
		},
		BedrockAccess: grantAllModels{},
	}
}

func bedrockAgentRuntimeRequestWithAccount(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	ctx := context.WithValue(req.Context(), ctxAccountID, bedrockAgentTestAccount)
	ctx = context.WithValue(ctx, ctxIdentity, "alice")
	ctx = context.WithValue(ctx, ctxPrincipalType, principalTypeUser)
	return req.WithContext(ctx)
}

func newTestKnowledgeBaseRecord(t *testing.T, gw *GatewayConfig, kbID, indexID string) {
	t.Helper()
	require.NoError(t, gw.BedrockAgentKB.Create(context.Background(), bedrockAgentTestAccount, handlers_ochrevector.KBRecord{
		ID: kbID, Name: "docs", Status: handlers_ochrevector.StateReady,
		EmbeddingModel: "amazon.titan-embed-text-v2:0", Dimension: 1024, IndexID: indexID,
	}))
}

func TestBedrockAgentRuntimeRequest_Retrieve_HappyPath(t *testing.T) {
	ts := newVLLMStub(t)
	vector := &fakeBedrockAgentVectorService{queryResp: handlers_ochrevector.QueryResponse{
		Results: []handlers_ochrevector.QueryResult{
			{Chunk: "the dragon lives in a cave", SourceKey: "docs/a.txt", Score: 0.9},
		},
	}}
	gw := newBedrockAgentRuntimeRequestGateway(t, vector, ts.URL)
	newTestKnowledgeBaseRecord(t, gw, "kb-1", "idx-1")

	body := `{"retrievalQuery": {"text": "dragons"}, "retrievalConfiguration": {"vectorSearchConfiguration": {"numberOfResults": 5}}}`
	req := bedrockAgentRuntimeRequestWithAccount(http.MethodPost, "/knowledgebases/kb-1/retrieve", body)
	w := httptest.NewRecorder()
	require.NoError(t, gw.BedrockAgentRuntime_Request(w, req))
	require.Equal(t, http.StatusOK, w.Code)

	var out bedrockagentruntime.RetrieveOutput
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.Len(t, out.RetrievalResults, 1)
	assert.Equal(t, "the dragon lives in a cave", *out.RetrievalResults[0].Content.Text)
	assert.Equal(t, "docs/a.txt", *out.RetrievalResults[0].Location.S3Location.Uri)

	require.NotNil(t, vector.queryReq)
	assert.Equal(t, "idx-1", vector.queryReq.IndexID)
	assert.Equal(t, 5, vector.queryReq.K)
}

func TestBedrockAgentRuntimeRequest_Retrieve_WithFilter(t *testing.T) {
	ts := newVLLMStub(t)
	vector := &fakeBedrockAgentVectorService{}
	gw := newBedrockAgentRuntimeRequestGateway(t, vector, ts.URL)
	newTestKnowledgeBaseRecord(t, gw, "kb-1", "idx-1")

	body := `{
		"retrievalQuery": {"text": "dragons"},
		"retrievalConfiguration": {"vectorSearchConfiguration": {"filter": {"equals": {"key": "genre", "value": "fiction"}}}}
	}`
	req := bedrockAgentRuntimeRequestWithAccount(http.MethodPost, "/knowledgebases/kb-1/retrieve", body)
	w := httptest.NewRecorder()
	require.NoError(t, gw.BedrockAgentRuntime_Request(w, req))
	require.Equal(t, http.StatusOK, w.Code)

	require.NotNil(t, vector.queryReq)
	require.NotNil(t, vector.queryReq.Filter)
	assert.Equal(t, handlers_ochrevector.FilterEquals, vector.queryReq.Filter.Op)
	assert.Equal(t, "genre", vector.queryReq.Filter.Key)
}

func TestBedrockAgentRuntimeRequest_Retrieve_UnknownKnowledgeBase(t *testing.T) {
	ts := newVLLMStub(t)
	gw := newBedrockAgentRuntimeRequestGateway(t, &fakeBedrockAgentVectorService{}, ts.URL)

	body := `{"retrievalQuery": {"text": "dragons"}}`
	req := bedrockAgentRuntimeRequestWithAccount(http.MethodPost, "/knowledgebases/does-not-exist/retrieve", body)
	w := httptest.NewRecorder()
	err := gw.BedrockAgentRuntime_Request(w, req)
	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorResourceNotFoundException))
}

func TestBedrockAgentRuntimeRequest_Retrieve_MalformedBodyReturnsValidationException(t *testing.T) {
	ts := newVLLMStub(t)
	gw := newBedrockAgentRuntimeRequestGateway(t, &fakeBedrockAgentVectorService{}, ts.URL)

	req := bedrockAgentRuntimeRequestWithAccount(http.MethodPost, "/knowledgebases/kb-1/retrieve", "{not-json")
	w := httptest.NewRecorder()
	err := gw.BedrockAgentRuntime_Request(w, req)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorValidationException, err.Error())
}

func TestBedrockAgentRuntimeRequest_RetrieveAndGenerate_HappyPath(t *testing.T) {
	ts := newVLLMStub(t)
	vector := &fakeBedrockAgentVectorService{queryResp: handlers_ochrevector.QueryResponse{
		Results: []handlers_ochrevector.QueryResult{
			{Chunk: "the dragon lives in a cave", SourceKey: "docs/a.txt", Score: 0.9},
		},
	}}
	gw := newBedrockAgentRuntimeRequestGateway(t, vector, ts.URL)
	newTestKnowledgeBaseRecord(t, gw, "kb-1", "idx-1")

	body := `{
		"input": {"text": "where does the dragon live?"},
		"retrieveAndGenerateConfiguration": {
			"type": "KNOWLEDGE_BASE",
			"knowledgeBaseConfiguration": {
				"knowledgeBaseId": "kb-1",
				"modelArn": "arn:aws:bedrock:us-east-1::foundation-model/` + bedrockTestLlamaModelID + `"
			}
		}
	}`
	req := bedrockAgentRuntimeRequestWithAccount(http.MethodPost, "/retrieveAndGenerate", body)
	w := httptest.NewRecorder()
	require.NoError(t, gw.BedrockAgentRuntime_Request(w, req))
	require.Equal(t, http.StatusOK, w.Code)

	var out bedrockagentruntime.RetrieveAndGenerateOutput
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, "hi from vllm", *out.Output.Text)
	require.NotEmpty(t, *out.SessionId)
	require.Len(t, out.Citations, 1)
	require.Len(t, out.Citations[0].RetrievedReferences, 1)
	assert.Equal(t, "the dragon lives in a cave", *out.Citations[0].RetrievedReferences[0].Content.Text)
}

func TestBedrockAgentRuntimeRequest_RetrieveAndGenerate_UnknownKnowledgeBase(t *testing.T) {
	ts := newVLLMStub(t)
	gw := newBedrockAgentRuntimeRequestGateway(t, &fakeBedrockAgentVectorService{}, ts.URL)

	body := `{
		"input": {"text": "q"},
		"retrieveAndGenerateConfiguration": {
			"type": "KNOWLEDGE_BASE",
			"knowledgeBaseConfiguration": {"knowledgeBaseId": "does-not-exist", "modelArn": "` + bedrockTestLlamaModelID + `"}
		}
	}`
	req := bedrockAgentRuntimeRequestWithAccount(http.MethodPost, "/retrieveAndGenerate", body)
	w := httptest.NewRecorder()
	err := gw.BedrockAgentRuntime_Request(w, req)
	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorResourceNotFoundException))
}

func TestBedrockAgentRuntimeRequest_RetrieveAndGenerate_MalformedBodyReturnsValidationException(t *testing.T) {
	ts := newVLLMStub(t)
	gw := newBedrockAgentRuntimeRequestGateway(t, &fakeBedrockAgentVectorService{}, ts.URL)

	req := bedrockAgentRuntimeRequestWithAccount(http.MethodPost, "/retrieveAndGenerate", "{not-json")
	w := httptest.NewRecorder()
	err := gw.BedrockAgentRuntime_Request(w, req)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorValidationException, err.Error())
}

func TestBedrockAgentRuntimeRequest_UnknownRouteReturnsInvalidAction(t *testing.T) {
	ts := newVLLMStub(t)
	gw := newBedrockAgentRuntimeRequestGateway(t, &fakeBedrockAgentVectorService{}, ts.URL)

	req := bedrockAgentRuntimeRequestWithAccount(http.MethodPatch, "/knowledgebases/kb-1/retrieve", "")
	w := httptest.NewRecorder()
	err := gw.BedrockAgentRuntime_Request(w, req)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAction, err.Error())
}

func TestBedrockAgentRuntimeRequest_MissingAccountIDReturnsInternalError(t *testing.T) {
	ts := newVLLMStub(t)
	gw := newBedrockAgentRuntimeRequestGateway(t, &fakeBedrockAgentVectorService{}, ts.URL)

	req := withTestIdentity(httptest.NewRequest(http.MethodPost, "/retrieveAndGenerate", nil))
	w := httptest.NewRecorder()
	// Account-less requests are rejected by the policy gate, which runs ahead
	// of the handler's own account guard.
	err := gw.BedrockAgentRuntime_Request(w, req)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInternalError, err.Error())
}

func TestBedrockAgentRuntimeRequest_NilDependenciesReturnServerInternal(t *testing.T) {
	gw := &GatewayConfig{IAMService: allowAllIAMService()}

	req := bedrockAgentRuntimeRequestWithAccount(http.MethodPost, "/knowledgebases/kb-1/retrieve", `{"retrievalQuery":{"text":"q"}}`)
	w := httptest.NewRecorder()
	err := gw.BedrockAgentRuntime_Request(w, req)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}
