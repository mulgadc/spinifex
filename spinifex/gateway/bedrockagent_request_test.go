// In-package: exercises BedrockAgent_Request's route table, dispatcher, and
// the per-route handler closures in bedrockAgentRoutes end-to-end over real
// HTTP requests, mirroring bedrock_request_test.go's harness for the sibling
// bedrock/bedrock-runtime services.
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

	"github.com/aws/aws-sdk-go/service/bedrockagent"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ochrevector "github.com/mulgadc/spinifex/spinifex/handlers/ochrevector"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newBedrockAgentRequestGateway builds a GatewayConfig with fresh
// gateway-owned KB/DataSource stores over a real (embedded) JetStream and an
// allow-everything IAMService, so the policy gate passes on a real evaluation
// rather than a bypass, mirroring newBedrockRequestGateway.
func newBedrockAgentRequestGateway(t *testing.T, vector handlers_ochrevector.VectorService) *GatewayConfig {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	return &GatewayConfig{
		IAMService:              allowAllIAMService(),
		BedrockAgentKB:          handlers_ochrevector.NewKBStore(js),
		BedrockAgentDataSources: handlers_ochrevector.NewDataSourceStore(js),
		BedrockAgentVector:      vector,
	}
}

func bedrockAgentRequestWithAccount(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	ctx := context.WithValue(req.Context(), ctxAccountID, bedrockAgentTestAccount)
	ctx = context.WithValue(ctx, ctxIdentity, "alice")
	ctx = context.WithValue(ctx, ctxPrincipalType, principalTypeUser)
	return req.WithContext(ctx)
}

const createKnowledgeBaseRequestBody = `{
	"name": "docs",
	"roleArn": "arn:aws:iam::111111111111:role/kb-role",
	"storageConfiguration": {"type": "OPENSEARCH_SERVERLESS"},
	"knowledgeBaseConfiguration": {
		"type": "VECTOR",
		"vectorKnowledgeBaseConfiguration": {
			"embeddingModelArn": "arn:aws:bedrock:us-east-1::foundation-model/amazon.titan-embed-text-v2:0",
			"embeddingModelConfiguration": {
				"bedrockEmbeddingModelConfiguration": {"dimensions": 1024}
			}
		}
	}
}`

// createTestKnowledgeBase drives CreateKnowledgeBase through the HTTP route
// so tests that need an existing knowledge base don't have to hand-build a
// KBRecord, and returns its generated id.
func createTestKnowledgeBase(t *testing.T, gw *GatewayConfig) string {
	t.Helper()
	req := bedrockAgentRequestWithAccount(http.MethodPut, "/knowledgebases/", createKnowledgeBaseRequestBody)
	w := httptest.NewRecorder()
	require.NoError(t, gw.BedrockAgent_Request(w, req))
	require.Equal(t, http.StatusOK, w.Code)
	var out bedrockagent.CreateKnowledgeBaseOutput
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	return *out.KnowledgeBase.KnowledgeBaseId
}

func TestBedrockAgentRequest_CreateKnowledgeBase(t *testing.T) {
	gw := newBedrockAgentRequestGateway(t, &fakeBedrockAgentVectorService{})

	req := bedrockAgentRequestWithAccount(http.MethodPut, "/knowledgebases/", createKnowledgeBaseRequestBody)
	w := httptest.NewRecorder()
	require.NoError(t, gw.BedrockAgent_Request(w, req))
	require.Equal(t, http.StatusOK, w.Code)

	var out bedrockagent.CreateKnowledgeBaseOutput
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, "docs", *out.KnowledgeBase.Name)
	assert.Equal(t, bedrockagent.KnowledgeBaseStatusActive, *out.KnowledgeBase.Status)
}

func TestBedrockAgentRequest_CreateKnowledgeBase_MalformedBodyReturnsValidationException(t *testing.T) {
	gw := newBedrockAgentRequestGateway(t, &fakeBedrockAgentVectorService{})

	req := bedrockAgentRequestWithAccount(http.MethodPut, "/knowledgebases/", "{not-json")
	w := httptest.NewRecorder()
	err := gw.BedrockAgent_Request(w, req)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorValidationException, err.Error())
}

func TestBedrockAgentRequest_GetKnowledgeBase(t *testing.T) {
	gw := newBedrockAgentRequestGateway(t, &fakeBedrockAgentVectorService{})
	id := createTestKnowledgeBase(t, gw)

	req := bedrockAgentRequestWithAccount(http.MethodGet, "/knowledgebases/"+id, "")
	w := httptest.NewRecorder()
	require.NoError(t, gw.BedrockAgent_Request(w, req))
	require.Equal(t, http.StatusOK, w.Code)

	var out bedrockagent.GetKnowledgeBaseOutput
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, id, *out.KnowledgeBase.KnowledgeBaseId)
}

func TestBedrockAgentRequest_GetKnowledgeBase_NotFound(t *testing.T) {
	gw := newBedrockAgentRequestGateway(t, &fakeBedrockAgentVectorService{})

	req := bedrockAgentRequestWithAccount(http.MethodGet, "/knowledgebases/does-not-exist", "")
	w := httptest.NewRecorder()
	err := gw.BedrockAgent_Request(w, req)
	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorResourceNotFoundException))
}

func TestBedrockAgentRequest_ListKnowledgeBases(t *testing.T) {
	gw := newBedrockAgentRequestGateway(t, &fakeBedrockAgentVectorService{})
	id := createTestKnowledgeBase(t, gw)

	req := bedrockAgentRequestWithAccount(http.MethodPost, "/knowledgebases/", "")
	w := httptest.NewRecorder()
	require.NoError(t, gw.BedrockAgent_Request(w, req))
	require.Equal(t, http.StatusOK, w.Code)

	var out bedrockagent.ListKnowledgeBasesOutput
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.Len(t, out.KnowledgeBaseSummaries, 1)
	assert.Equal(t, id, *out.KnowledgeBaseSummaries[0].KnowledgeBaseId)
}

const createDataSourceRequestBody = `{
	"name": "s3-docs",
	"dataSourceConfiguration": {
		"type": "S3",
		"s3Configuration": {
			"bucketArn": "arn:aws:s3:::my-bucket",
			"inclusionPrefixes": ["docs/"]
		}
	},
	"vectorIngestionConfiguration": {
		"chunkingConfiguration": {
			"chunkingStrategy": "FIXED_SIZE",
			"fixedSizeChunkingConfiguration": {"maxTokens": 300, "overlapPercentage": 20}
		}
	}
}`

func createTestDataSource(t *testing.T, gw *GatewayConfig, kbID string) string {
	t.Helper()
	req := bedrockAgentRequestWithAccount(http.MethodPut, "/knowledgebases/"+kbID+"/datasources/", createDataSourceRequestBody)
	w := httptest.NewRecorder()
	require.NoError(t, gw.BedrockAgent_Request(w, req))
	require.Equal(t, http.StatusOK, w.Code)
	var out bedrockagent.CreateDataSourceOutput
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	return *out.DataSource.DataSourceId
}

func TestBedrockAgentRequest_CreateDataSource(t *testing.T) {
	gw := newBedrockAgentRequestGateway(t, &fakeBedrockAgentVectorService{})
	kbID := createTestKnowledgeBase(t, gw)

	req := bedrockAgentRequestWithAccount(http.MethodPut, "/knowledgebases/"+kbID+"/datasources/", createDataSourceRequestBody)
	w := httptest.NewRecorder()
	require.NoError(t, gw.BedrockAgent_Request(w, req))
	require.Equal(t, http.StatusOK, w.Code)

	var out bedrockagent.CreateDataSourceOutput
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, "s3-docs", *out.DataSource.Name)
	assert.Equal(t, "arn:aws:s3:::my-bucket", *out.DataSource.DataSourceConfiguration.S3Configuration.BucketArn)
}

func TestBedrockAgentRequest_CreateDataSource_UnknownKnowledgeBaseNotFound(t *testing.T) {
	gw := newBedrockAgentRequestGateway(t, &fakeBedrockAgentVectorService{})

	req := bedrockAgentRequestWithAccount(http.MethodPut, "/knowledgebases/does-not-exist/datasources/", createDataSourceRequestBody)
	w := httptest.NewRecorder()
	err := gw.BedrockAgent_Request(w, req)
	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorResourceNotFoundException))
}

func TestBedrockAgentRequest_GetDataSource(t *testing.T) {
	gw := newBedrockAgentRequestGateway(t, &fakeBedrockAgentVectorService{})
	kbID := createTestKnowledgeBase(t, gw)
	dsID := createTestDataSource(t, gw, kbID)

	req := bedrockAgentRequestWithAccount(http.MethodGet, "/knowledgebases/"+kbID+"/datasources/"+dsID, "")
	w := httptest.NewRecorder()
	require.NoError(t, gw.BedrockAgent_Request(w, req))
	require.Equal(t, http.StatusOK, w.Code)

	var out bedrockagent.GetDataSourceOutput
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, dsID, *out.DataSource.DataSourceId)
}

func TestBedrockAgentRequest_ListDataSources(t *testing.T) {
	gw := newBedrockAgentRequestGateway(t, &fakeBedrockAgentVectorService{})
	kbID := createTestKnowledgeBase(t, gw)
	dsID := createTestDataSource(t, gw, kbID)

	req := bedrockAgentRequestWithAccount(http.MethodPost, "/knowledgebases/"+kbID+"/datasources/", "")
	w := httptest.NewRecorder()
	require.NoError(t, gw.BedrockAgent_Request(w, req))
	require.Equal(t, http.StatusOK, w.Code)

	var out bedrockagent.ListDataSourcesOutput
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.Len(t, out.DataSourceSummaries, 1)
	assert.Equal(t, dsID, *out.DataSourceSummaries[0].DataSourceId)
}

func TestBedrockAgentRequest_DeleteDataSource(t *testing.T) {
	gw := newBedrockAgentRequestGateway(t, &fakeBedrockAgentVectorService{})
	kbID := createTestKnowledgeBase(t, gw)
	dsID := createTestDataSource(t, gw, kbID)

	req := bedrockAgentRequestWithAccount(http.MethodDelete, "/knowledgebases/"+kbID+"/datasources/"+dsID, "")
	w := httptest.NewRecorder()
	require.NoError(t, gw.BedrockAgent_Request(w, req))
	require.Equal(t, http.StatusOK, w.Code)

	getReq := bedrockAgentRequestWithAccount(http.MethodGet, "/knowledgebases/"+kbID+"/datasources/"+dsID, "")
	w2 := httptest.NewRecorder()
	err := gw.BedrockAgent_Request(w2, getReq)
	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorResourceNotFoundException))
}

func TestBedrockAgentRequest_DeleteKnowledgeBase(t *testing.T) {
	gw := newBedrockAgentRequestGateway(t, &fakeBedrockAgentVectorService{})
	kbID := createTestKnowledgeBase(t, gw)
	createTestDataSource(t, gw, kbID)

	req := bedrockAgentRequestWithAccount(http.MethodDelete, "/knowledgebases/"+kbID, "")
	w := httptest.NewRecorder()
	require.NoError(t, gw.BedrockAgent_Request(w, req))
	require.Equal(t, http.StatusOK, w.Code)

	getReq := bedrockAgentRequestWithAccount(http.MethodGet, "/knowledgebases/"+kbID, "")
	w2 := httptest.NewRecorder()
	err := gw.BedrockAgent_Request(w2, getReq)
	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorResourceNotFoundException))
}

func TestBedrockAgentRequest_StartAndListAndGetIngestionJob(t *testing.T) {
	vector := &fakeBedrockAgentVectorService{ingestResp: handlers_ochrevector.IngestResponse{
		Job: handlers_ochrevector.JobRecord{ID: "job-1", State: handlers_ochrevector.JobStatePending},
	}}
	gw := newBedrockAgentRequestGateway(t, vector)
	kbID := createTestKnowledgeBase(t, gw)
	dsID := createTestDataSource(t, gw, kbID)

	// vector.Ingest doesn't stamp IndexID/Source on the returned job itself
	// (that's jobRecordToOutput's job, driven by the KB's own bound index),
	// but ListJobs/GetIngestionJob key off the fake's own scripted response,
	// so both are seeded from the stores' own records here: IndexID from the
	// KB, Source from the data source (sourceSpecMatchesDataSource's match key).
	kbRec, err := gw.BedrockAgentKB.Get(context.Background(), bedrockAgentTestAccount, kbID)
	require.NoError(t, err)
	dsRec, err := gw.BedrockAgentDataSources.Get(context.Background(), bedrockAgentTestAccount, dsID)
	require.NoError(t, err)
	vector.ingestResp.Job.IndexID = kbRec.IndexID
	vector.ingestResp.Job.Source = dsRec.Source
	vector.describeJobResp = handlers_ochrevector.DescribeJobResponse{Job: vector.ingestResp.Job}
	vector.listJobsResp = handlers_ochrevector.ListJobsResponse{Jobs: []handlers_ochrevector.JobRecord{vector.ingestResp.Job}}

	startReq := bedrockAgentRequestWithAccount(http.MethodPut, "/knowledgebases/"+kbID+"/datasources/"+dsID+"/ingestionjobs/", "")
	w := httptest.NewRecorder()
	require.NoError(t, gw.BedrockAgent_Request(w, startReq))
	require.Equal(t, http.StatusOK, w.Code)
	var startOut bedrockagent.StartIngestionJobOutput
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &startOut))
	assert.Equal(t, "job-1", *startOut.IngestionJob.IngestionJobId)

	listReq := bedrockAgentRequestWithAccount(http.MethodPost, "/knowledgebases/"+kbID+"/datasources/"+dsID+"/ingestionjobs/", "")
	w2 := httptest.NewRecorder()
	require.NoError(t, gw.BedrockAgent_Request(w2, listReq))
	require.Equal(t, http.StatusOK, w2.Code)
	var listOut bedrockagent.ListIngestionJobsOutput
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &listOut))
	require.Len(t, listOut.IngestionJobSummaries, 1)
	assert.Equal(t, "job-1", *listOut.IngestionJobSummaries[0].IngestionJobId)

	getReq := bedrockAgentRequestWithAccount(http.MethodGet, "/knowledgebases/"+kbID+"/datasources/"+dsID+"/ingestionjobs/job-1", "")
	w3 := httptest.NewRecorder()
	require.NoError(t, gw.BedrockAgent_Request(w3, getReq))
	require.Equal(t, http.StatusOK, w3.Code)
	var getOut bedrockagent.GetIngestionJobOutput
	require.NoError(t, json.Unmarshal(w3.Body.Bytes(), &getOut))
	assert.Equal(t, "job-1", *getOut.IngestionJob.IngestionJobId)
}

func TestBedrockAgentRequest_UnknownRouteReturnsInvalidAction(t *testing.T) {
	gw := newBedrockAgentRequestGateway(t, &fakeBedrockAgentVectorService{})

	req := bedrockAgentRequestWithAccount(http.MethodPatch, "/knowledgebases/", "")
	w := httptest.NewRecorder()
	err := gw.BedrockAgent_Request(w, req)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAction, err.Error())
}

func TestBedrockAgentRequest_MissingAccountIDReturnsInternalError(t *testing.T) {
	gw := newBedrockAgentRequestGateway(t, &fakeBedrockAgentVectorService{})

	req := withTestIdentity(httptest.NewRequest(http.MethodPost, "/knowledgebases/", nil))
	w := httptest.NewRecorder()
	// Account-less requests are rejected by the policy gate, which runs ahead
	// of the handler's own account guard.
	err := gw.BedrockAgent_Request(w, req)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInternalError, err.Error())
}

func TestBedrockAgentRequest_NilDependenciesReturnServerInternal(t *testing.T) {
	gw := &GatewayConfig{IAMService: allowAllIAMService()}

	req := bedrockAgentRequestWithAccount(http.MethodPost, "/knowledgebases/", "")
	w := httptest.NewRecorder()
	err := gw.BedrockAgent_Request(w, req)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}
