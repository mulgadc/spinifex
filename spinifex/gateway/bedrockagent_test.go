// In-package: exercises the unexported AWS<->internal mapper helpers
// (kbStatusToAWS, embeddingModelIDFromARN, sourceSpecMatchesDataSource, ...)
// directly, alongside the operation-level tests through the exported
// Create/Get/List/Delete functions.
//
//test:in-package
package gateway

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrockagent"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ochrevector "github.com/mulgadc/spinifex/spinifex/handlers/ochrevector"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const bedrockAgentTestAccount = "111111111111"

// fakeBedrockAgentVectorService serves scripted responses so bedrockagent.go's
// mapping logic can be exercised without a daemon or a live NATS connection,
// mirroring cmd/spinifex/cmd's fakeVectorService for this package.
type fakeBedrockAgentVectorService struct {
	createIndexReq *handlers_ochrevector.CreateIndexRequest
	createIndexErr error

	deletedIndexIDs []string
	deleteIndexErr  error

	ingestReq  *handlers_ochrevector.IngestRequest
	ingestResp handlers_ochrevector.IngestResponse
	ingestErr  error

	describeJobResp handlers_ochrevector.DescribeJobResponse
	describeJobErr  error

	listJobsResp handlers_ochrevector.ListJobsResponse
	listJobsErr  error

	queryReq  *handlers_ochrevector.QueryRequest
	queryResp handlers_ochrevector.QueryResponse
	queryErr  error
}

func (f *fakeBedrockAgentVectorService) CreateIndex(_ context.Context, req *handlers_ochrevector.CreateIndexRequest, _ string) (*handlers_ochrevector.CreateIndexResponse, error) {
	f.createIndexReq = req
	if f.createIndexErr != nil {
		return nil, f.createIndexErr
	}
	return &handlers_ochrevector.CreateIndexResponse{Index: handlers_ochrevector.Record{
		ID: req.IndexID, Name: req.Name, Dimension: req.Dimension,
		EmbeddingModel: req.EmbeddingModel, State: handlers_ochrevector.StateReady,
	}}, nil
}

func (f *fakeBedrockAgentVectorService) DeleteIndex(_ context.Context, req *handlers_ochrevector.DeleteIndexRequest, _ string) (*handlers_ochrevector.DeleteIndexResponse, error) {
	f.deletedIndexIDs = append(f.deletedIndexIDs, req.IndexID)
	if f.deleteIndexErr != nil {
		return nil, f.deleteIndexErr
	}
	return &handlers_ochrevector.DeleteIndexResponse{}, nil
}

func (f *fakeBedrockAgentVectorService) ListIndexes(_ context.Context, _ *handlers_ochrevector.ListIndexesRequest, _ string) (*handlers_ochrevector.ListIndexesResponse, error) {
	return &handlers_ochrevector.ListIndexesResponse{}, nil
}

func (f *fakeBedrockAgentVectorService) Ingest(_ context.Context, req *handlers_ochrevector.IngestRequest, _ string) (*handlers_ochrevector.IngestResponse, error) {
	f.ingestReq = req
	if f.ingestErr != nil {
		return nil, f.ingestErr
	}
	resp := f.ingestResp
	return &resp, nil
}

func (f *fakeBedrockAgentVectorService) DescribeJob(_ context.Context, _ *handlers_ochrevector.DescribeJobRequest, _ string) (*handlers_ochrevector.DescribeJobResponse, error) {
	if f.describeJobErr != nil {
		return nil, f.describeJobErr
	}
	resp := f.describeJobResp
	return &resp, nil
}

func (f *fakeBedrockAgentVectorService) ListJobs(_ context.Context, _ *handlers_ochrevector.ListJobsRequest, _ string) (*handlers_ochrevector.ListJobsResponse, error) {
	if f.listJobsErr != nil {
		return nil, f.listJobsErr
	}
	resp := f.listJobsResp
	return &resp, nil
}

func (f *fakeBedrockAgentVectorService) Query(_ context.Context, req *handlers_ochrevector.QueryRequest, _ string) (*handlers_ochrevector.QueryResponse, error) {
	f.queryReq = req
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	resp := f.queryResp
	return &resp, nil
}

var _ handlers_ochrevector.VectorService = (*fakeBedrockAgentVectorService)(nil)

func newBedrockAgentTestStores(t *testing.T) (*handlers_ochrevector.KBStore, *handlers_ochrevector.DataSourceStore) {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	return handlers_ochrevector.NewKBStore(js), handlers_ochrevector.NewDataSourceStore(js)
}

func TestEmbeddingModelIDFromARN(t *testing.T) {
	cases := []struct{ arn, want string }{
		{"arn:aws:bedrock:us-east-1::foundation-model/amazon.titan-embed-text-v2:0", "amazon.titan-embed-text-v2:0"},
		{"amazon.titan-embed-text-v2:0", "amazon.titan-embed-text-v2:0"},
		{"", ""},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, embeddingModelIDFromARN(tc.arn))
	}
}

func TestBucketNameFromS3ARN_And_FormatS3BucketARN_RoundTrip(t *testing.T) {
	assert.Equal(t, "my-bucket", bucketNameFromS3ARN("arn:aws:s3:::my-bucket"))
	assert.Equal(t, "my-bucket", bucketNameFromS3ARN("my-bucket"))
	assert.Equal(t, "arn:aws:s3:::my-bucket", formatS3BucketARN("my-bucket"))
	assert.Equal(t, "my-bucket", bucketNameFromS3ARN(formatS3BucketARN("my-bucket")))
}

func TestKBStatusToAWS(t *testing.T) {
	cases := []struct{ status, want string }{
		{handlers_ochrevector.StateReady, bedrockagent.KnowledgeBaseStatusActive},
		{handlers_ochrevector.StateCreating, bedrockagent.KnowledgeBaseStatusCreating},
		{handlers_ochrevector.StateDeleting, bedrockagent.KnowledgeBaseStatusDeleting},
		{handlers_ochrevector.StateStale, bedrockagent.KnowledgeBaseStatusFailed},
		{"unknown", bedrockagent.KnowledgeBaseStatusFailed},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, kbStatusToAWS(tc.status))
	}
}

func TestDataSourceStatusToAWS(t *testing.T) {
	assert.Equal(t, bedrockagent.DataSourceStatusDeleting, dataSourceStatusToAWS(handlers_ochrevector.StateDeleting))
	assert.Equal(t, bedrockagent.DataSourceStatusAvailable, dataSourceStatusToAWS(handlers_ochrevector.StateReady))
}

func TestJobStateToAWS(t *testing.T) {
	cases := []struct{ state, want string }{
		{handlers_ochrevector.JobStatePending, bedrockagent.IngestionJobStatusStarting},
		{handlers_ochrevector.JobStateRunning, bedrockagent.IngestionJobStatusInProgress},
		{handlers_ochrevector.JobStateReady, bedrockagent.IngestionJobStatusComplete},
		{handlers_ochrevector.JobStateFailed, bedrockagent.IngestionJobStatusFailed},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, jobStateToAWS(tc.state))
	}
}

func TestTranslateVectorErr(t *testing.T) {
	assert.NoError(t, translateVectorErr(nil))
	assert.True(t, awserrors.IsErrorCode(translateVectorErr(handlers_ochrevector.ErrIndexNotFound), awserrors.ErrorResourceNotFoundException))
	assert.True(t, awserrors.IsErrorCode(translateVectorErr(handlers_ochrevector.ErrJobNotFound), awserrors.ErrorResourceNotFoundException))
	assert.True(t, awserrors.IsErrorCode(translateVectorErr(handlers_ochrevector.ErrIndexExists), awserrors.ErrorConflictException))
}

func TestSourceSpecMatchesDataSource(t *testing.T) {
	ds := handlers_ochrevector.SourceSpec{Bucket: "b1", Prefix: "docs/"}
	assert.True(t, sourceSpecMatchesDataSource(handlers_ochrevector.SourceSpec{Bucket: "b1", Prefix: "docs/"}, ds))
	assert.False(t, sourceSpecMatchesDataSource(handlers_ochrevector.SourceSpec{Bucket: "other", Prefix: "docs/"}, ds))
	assert.False(t, sourceSpecMatchesDataSource(handlers_ochrevector.SourceSpec{Bucket: "b1", Prefix: "other/"}, ds))
}

func validCreateKBInput() *bedrockagent.CreateKnowledgeBaseInput {
	return &bedrockagent.CreateKnowledgeBaseInput{
		Name:                 aws.String("docs"),
		RoleArn:              aws.String("arn:aws:iam::111111111111:role/kb-role"),
		StorageConfiguration: &bedrockagent.StorageConfiguration{Type: aws.String("OPENSEARCH_SERVERLESS")},
		KnowledgeBaseConfiguration: &bedrockagent.KnowledgeBaseConfiguration{
			Type: aws.String(bedrockagent.KnowledgeBaseTypeVector),
			VectorKnowledgeBaseConfiguration: &bedrockagent.VectorKnowledgeBaseConfiguration{
				EmbeddingModelArn: aws.String("arn:aws:bedrock:us-east-1::foundation-model/amazon.titan-embed-text-v2:0"),
				EmbeddingModelConfiguration: &bedrockagent.EmbeddingModelConfiguration{
					BedrockEmbeddingModelConfiguration: &bedrockagent.BedrockEmbeddingModelConfiguration{Dimensions: aws.Int64(1024)},
				},
			},
		},
	}
}

func TestCreateKnowledgeBase_MapsEmbeddingModelAndDimension(t *testing.T) {
	kb, _ := newBedrockAgentTestStores(t)
	vector := &fakeBedrockAgentVectorService{}
	ctx := context.Background()

	out, err := CreateKnowledgeBase(ctx, bedrockAgentTestAccount, "us-east-1", kb, vector, validCreateKBInput())
	require.NoError(t, err)

	require.NotNil(t, vector.createIndexReq)
	assert.Equal(t, 1024, vector.createIndexReq.Dimension)
	assert.Equal(t, "amazon.titan-embed-text-v2:0", vector.createIndexReq.EmbeddingModel)

	assert.Equal(t, bedrockagent.KnowledgeBaseStatusActive, aws.StringValue(out.KnowledgeBase.Status))
	assert.Equal(t, "docs", aws.StringValue(out.KnowledgeBase.Name))
	dims := out.KnowledgeBase.KnowledgeBaseConfiguration.VectorKnowledgeBaseConfiguration.EmbeddingModelConfiguration.BedrockEmbeddingModelConfiguration.Dimensions
	assert.Equal(t, int64(1024), aws.Int64Value(dims))
	assert.Contains(t, aws.StringValue(out.KnowledgeBase.KnowledgeBaseArn), "knowledge-base/")
}

func TestCreateKnowledgeBase_MissingDimensionsIsValidationException(t *testing.T) {
	kb, _ := newBedrockAgentTestStores(t)
	vector := &fakeBedrockAgentVectorService{}
	input := validCreateKBInput()
	input.KnowledgeBaseConfiguration.VectorKnowledgeBaseConfiguration.EmbeddingModelConfiguration = nil

	_, err := CreateKnowledgeBase(context.Background(), bedrockAgentTestAccount, "us-east-1", kb, vector, input)
	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorValidationException))
}

// TestCreateKnowledgeBase_RollbackFailureIsLoggedNotSwallowed proves that
// when kb.Create fails (forcing the just-created index's rollback delete)
// and that rollback delete itself also fails, the caller still sees kb.Create's
// own error unchanged -- not the rollback's -- while the rollback failure is
// logged at Error level with the orphaned index id, so an orphan stays
// observable instead of vanishing silently.
func TestCreateKnowledgeBase_RollbackFailureIsLoggedNotSwallowed(t *testing.T) {
	var buf strings.Builder
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(slog.New(slog.DiscardHandler)) })

	// A KBStore with no JetStream client makes kb.Create fail deterministically
	// (independent of the randomly generated knowledge base id), so the
	// rollback path is reached without needing to force a real id collision.
	kb := handlers_ochrevector.NewKBStore(nil)
	vector := &fakeBedrockAgentVectorService{deleteIndexErr: errors.New("rollback backend unavailable")}

	_, err := CreateKnowledgeBase(context.Background(), bedrockAgentTestAccount, "us-east-1", kb, vector, validCreateKBInput())
	require.Error(t, err)
	// The caller-visible error is kb.Create's own failure, not the rollback's.
	assert.Contains(t, err.Error(), "knowledge base store has no JetStream client configured")
	assert.NotContains(t, err.Error(), "rollback backend unavailable")

	require.Len(t, vector.deletedIndexIDs, 1, "rollback delete must still be attempted")
	orphanedIndexID := vector.deletedIndexIDs[0]

	logOutput := buf.String()
	assert.Contains(t, logOutput, "level=ERROR")
	assert.Contains(t, logOutput, "orphaned index")
	assert.Contains(t, logOutput, orphanedIndexID)
	assert.Contains(t, logOutput, "rollback backend unavailable")
}

func TestDeleteKnowledgeBase_CascadesDataSourcesAndDeletesBoundIndex(t *testing.T) {
	kb, ds := newBedrockAgentTestStores(t)
	ctx := context.Background()

	require.NoError(t, kb.Create(ctx, bedrockAgentTestAccount, handlers_ochrevector.KBRecord{ID: "kb-1", IndexID: "idx-1", Status: handlers_ochrevector.StateReady}))
	require.NoError(t, ds.Create(ctx, bedrockAgentTestAccount, handlers_ochrevector.DataSourceRecord{ID: "ds-1", KnowledgeBaseID: "kb-1"}))
	require.NoError(t, ds.Create(ctx, bedrockAgentTestAccount, handlers_ochrevector.DataSourceRecord{ID: "ds-2", KnowledgeBaseID: "kb-1"}))

	vector := &fakeBedrockAgentVectorService{}
	_, err := DeleteKnowledgeBase(ctx, bedrockAgentTestAccount, kb, ds, vector, &bedrockagent.DeleteKnowledgeBaseInput{KnowledgeBaseId: aws.String("kb-1")})
	require.NoError(t, err)

	assert.Equal(t, []string{"idx-1"}, vector.deletedIndexIDs)

	remaining, err := ds.ListByKnowledgeBase(ctx, bedrockAgentTestAccount, "kb-1")
	require.NoError(t, err)
	assert.Empty(t, remaining)

	got, err := kb.Get(ctx, bedrockAgentTestAccount, "kb-1")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestDeleteKnowledgeBase_UnknownIDReturnsNotFound(t *testing.T) {
	kb, ds := newBedrockAgentTestStores(t)
	vector := &fakeBedrockAgentVectorService{}
	_, err := DeleteKnowledgeBase(context.Background(), bedrockAgentTestAccount, kb, ds, vector, &bedrockagent.DeleteKnowledgeBaseInput{KnowledgeBaseId: aws.String("does-not-exist")})
	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorResourceNotFoundException))
}

// TestDeleteKnowledgeBase_TreatsMissingIndexAsAlreadyDeleted proves a bound
// index that is already gone (ErrIndexNotFound) does not abort the delete
// before KBStore.Delete runs: a KB record must never be left dangling,
// pointing at an index that no longer exists, just because that index
// happened to be deleted (or never provisioned) out from under it. Delete is
// idempotent w.r.t. a missing index, mirroring KBStore.Delete's own
// idempotent contract.
func TestDeleteKnowledgeBase_TreatsMissingIndexAsAlreadyDeleted(t *testing.T) {
	kb, ds := newBedrockAgentTestStores(t)
	ctx := context.Background()
	require.NoError(t, kb.Create(ctx, bedrockAgentTestAccount, handlers_ochrevector.KBRecord{ID: "kb-1", IndexID: "idx-gone", Status: handlers_ochrevector.StateReady}))

	vector := &fakeBedrockAgentVectorService{deleteIndexErr: handlers_ochrevector.ErrIndexNotFound}
	_, err := DeleteKnowledgeBase(ctx, bedrockAgentTestAccount, kb, ds, vector, &bedrockagent.DeleteKnowledgeBaseInput{KnowledgeBaseId: aws.String("kb-1")})
	require.NoError(t, err)

	got, err := kb.Get(ctx, bedrockAgentTestAccount, "kb-1")
	require.NoError(t, err)
	assert.Nil(t, got, "the KB record must not dangle once its bound index is confirmed gone")
}

// TestDeleteKnowledgeBase_GenuineIndexDeleteErrorStillAborts proves the
// ErrIndexNotFound tolerance above is narrow: any other DeleteIndex failure
// must still abort before KBStore.Delete runs, so the KB record survives to
// be retried.
func TestDeleteKnowledgeBase_GenuineIndexDeleteErrorStillAborts(t *testing.T) {
	kb, ds := newBedrockAgentTestStores(t)
	ctx := context.Background()
	require.NoError(t, kb.Create(ctx, bedrockAgentTestAccount, handlers_ochrevector.KBRecord{ID: "kb-1", IndexID: "idx-1", Status: handlers_ochrevector.StateReady}))

	vector := &fakeBedrockAgentVectorService{deleteIndexErr: errors.New("backend unavailable")}
	_, err := DeleteKnowledgeBase(ctx, bedrockAgentTestAccount, kb, ds, vector, &bedrockagent.DeleteKnowledgeBaseInput{KnowledgeBaseId: aws.String("kb-1")})
	require.Error(t, err)

	got, err := kb.Get(ctx, bedrockAgentTestAccount, "kb-1")
	require.NoError(t, err)
	assert.NotNil(t, got, "a genuine index-delete failure must leave the KB record in place for retry")
}

func createDataSourceInput(chunking *bedrockagent.VectorIngestionConfiguration) *bedrockagent.CreateDataSourceInput {
	return &bedrockagent.CreateDataSourceInput{
		KnowledgeBaseId: aws.String("kb-1"),
		Name:            aws.String("s3-docs"),
		DataSourceConfiguration: &bedrockagent.DataSourceConfiguration{
			Type: aws.String(bedrockagent.DataSourceTypeS3),
			S3Configuration: &bedrockagent.S3DataSourceConfiguration{
				BucketArn:         aws.String("arn:aws:s3:::my-bucket"),
				InclusionPrefixes: []*string{aws.String("docs/")},
			},
		},
		VectorIngestionConfiguration: chunking,
	}
}

// D3: ChunkOverlap = round(maxTokens * overlapPercentage/100).
func TestCreateDataSource_ChunkingUnitConversion(t *testing.T) {
	kb, ds := newBedrockAgentTestStores(t)
	ctx := context.Background()
	require.NoError(t, kb.Create(ctx, bedrockAgentTestAccount, handlers_ochrevector.KBRecord{
		ID: "kb-1", IndexID: "idx-1", Status: handlers_ochrevector.StateReady,
		EmbeddingModel: "titan", Dimension: 1024,
	}))

	input := createDataSourceInput(&bedrockagent.VectorIngestionConfiguration{
		ChunkingConfiguration: &bedrockagent.ChunkingConfiguration{
			ChunkingStrategy: aws.String(bedrockagent.ChunkingStrategyFixedSize),
			FixedSizeChunkingConfiguration: &bedrockagent.FixedSizeChunkingConfiguration{
				MaxTokens:         aws.Int64(300),
				OverlapPercentage: aws.Int64(20),
			},
		},
	})

	out, err := CreateDataSource(ctx, bedrockAgentTestAccount, "us-east-1", kb, ds, input)
	require.NoError(t, err)

	rec, err := ds.Get(ctx, bedrockAgentTestAccount, aws.StringValue(out.DataSource.DataSourceId))
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, 300, rec.Source.ChunkSize)
	assert.Equal(t, 60, rec.Source.ChunkOverlap)
	assert.Equal(t, "my-bucket", rec.Source.Bucket)
	assert.Equal(t, "docs/", rec.Source.Prefix)
	// EmbeddingModel/Dimension are inherited from the bound knowledge base.
	assert.Equal(t, "titan", rec.Source.EmbeddingModel)
	assert.Equal(t, 1024, rec.Source.Dimension)

	assert.Equal(t, "arn:aws:s3:::my-bucket", aws.StringValue(out.DataSource.DataSourceConfiguration.S3Configuration.BucketArn))
}

// D3: an omitted chunking block leaves ChunkSize/ChunkOverlap zero, so .9's
// own ingest path applies its defaults rather than this layer inventing one.
func TestCreateDataSource_OmittedChunkingLeavesZeroValues(t *testing.T) {
	kb, ds := newBedrockAgentTestStores(t)
	ctx := context.Background()
	require.NoError(t, kb.Create(ctx, bedrockAgentTestAccount, handlers_ochrevector.KBRecord{ID: "kb-1", IndexID: "idx-1", Status: handlers_ochrevector.StateReady}))

	out, err := CreateDataSource(ctx, bedrockAgentTestAccount, "us-east-1", kb, ds, createDataSourceInput(nil))
	require.NoError(t, err)

	rec, err := ds.Get(ctx, bedrockAgentTestAccount, aws.StringValue(out.DataSource.DataSourceId))
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Zero(t, rec.Source.ChunkSize)
	assert.Zero(t, rec.Source.ChunkOverlap)
}

func TestCreateDataSource_RejectsNonS3Type(t *testing.T) {
	kb, ds := newBedrockAgentTestStores(t)
	ctx := context.Background()
	require.NoError(t, kb.Create(ctx, bedrockAgentTestAccount, handlers_ochrevector.KBRecord{ID: "kb-1", IndexID: "idx-1"}))

	input := createDataSourceInput(nil)
	input.DataSourceConfiguration.Type = aws.String("WEB")
	input.DataSourceConfiguration.S3Configuration = nil

	_, err := CreateDataSource(ctx, bedrockAgentTestAccount, "us-east-1", kb, ds, input)
	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorValidationException))
}

func TestCreateDataSource_UnknownKnowledgeBaseIsNotFound(t *testing.T) {
	kb, ds := newBedrockAgentTestStores(t)
	_, err := CreateDataSource(context.Background(), bedrockAgentTestAccount, "us-east-1", kb, ds, createDataSourceInput(nil))
	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorResourceNotFoundException))
}

func TestStartIngestionJob_BuildsIngestRequestFromDataSourceAgainstBoundIndex(t *testing.T) {
	kb, ds := newBedrockAgentTestStores(t)
	ctx := context.Background()
	require.NoError(t, kb.Create(ctx, bedrockAgentTestAccount, handlers_ochrevector.KBRecord{ID: "kb-1", IndexID: "idx-1", Status: handlers_ochrevector.StateReady}))
	require.NoError(t, ds.Create(ctx, bedrockAgentTestAccount, handlers_ochrevector.DataSourceRecord{
		ID: "ds-1", KnowledgeBaseID: "kb-1",
		Source: handlers_ochrevector.SourceSpec{Bucket: "b1", Prefix: "p1"},
	}))

	vector := &fakeBedrockAgentVectorService{ingestResp: handlers_ochrevector.IngestResponse{
		Job: handlers_ochrevector.JobRecord{ID: "job-1", IndexID: "idx-1", State: handlers_ochrevector.JobStatePending},
	}}
	out, err := StartIngestionJob(ctx, bedrockAgentTestAccount, kb, ds, vector, &bedrockagent.StartIngestionJobInput{
		KnowledgeBaseId: aws.String("kb-1"), DataSourceId: aws.String("ds-1"),
	})
	require.NoError(t, err)

	require.NotNil(t, vector.ingestReq)
	assert.Equal(t, "idx-1", vector.ingestReq.IndexID)
	assert.Equal(t, "b1", vector.ingestReq.Source.Bucket)
	assert.Equal(t, "p1", vector.ingestReq.Source.Prefix)
	assert.Equal(t, bedrockagent.IngestionJobStatusStarting, aws.StringValue(out.IngestionJob.Status))
}

func TestGetIngestionJob_RejectsJobFromForeignIndex(t *testing.T) {
	kb, ds := newBedrockAgentTestStores(t)
	ctx := context.Background()
	require.NoError(t, kb.Create(ctx, bedrockAgentTestAccount, handlers_ochrevector.KBRecord{ID: "kb-1", IndexID: "idx-1"}))
	require.NoError(t, ds.Create(ctx, bedrockAgentTestAccount, handlers_ochrevector.DataSourceRecord{
		ID: "ds-1", KnowledgeBaseID: "kb-1",
		Source: handlers_ochrevector.SourceSpec{Bucket: "b1", Prefix: "p1"},
	}))

	vector := &fakeBedrockAgentVectorService{describeJobResp: handlers_ochrevector.DescribeJobResponse{
		Job: handlers_ochrevector.JobRecord{ID: "job-1", IndexID: "idx-other", Source: handlers_ochrevector.SourceSpec{Bucket: "b1", Prefix: "p1"}},
	}}
	_, err := GetIngestionJob(ctx, bedrockAgentTestAccount, kb, ds, vector, &bedrockagent.GetIngestionJobInput{
		KnowledgeBaseId: aws.String("kb-1"), DataSourceId: aws.String("ds-1"), IngestionJobId: aws.String("job-1"),
	})
	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorResourceNotFoundException))
}

// TestGetIngestionJob_RejectsWrongDataSource proves a job addressed through
// the wrong dataSourceId path segment is rejected even when its IndexID
// matches the knowledge base: a job that really belongs to one data source
// must not be readable by guessing a second, unrelated data source's id
// under the same KB. Regression test for the ownership check ListIngestionJobs
// already had but GetIngestionJob was missing.
func TestGetIngestionJob_RejectsWrongDataSource(t *testing.T) {
	kb, ds := newBedrockAgentTestStores(t)
	ctx := context.Background()
	require.NoError(t, kb.Create(ctx, bedrockAgentTestAccount, handlers_ochrevector.KBRecord{ID: "kb-1", IndexID: "idx-1"}))
	require.NoError(t, ds.Create(ctx, bedrockAgentTestAccount, handlers_ochrevector.DataSourceRecord{
		ID: "ds-a", KnowledgeBaseID: "kb-1",
		Source: handlers_ochrevector.SourceSpec{Bucket: "bucket-a", Prefix: "a/"},
	}))
	require.NoError(t, ds.Create(ctx, bedrockAgentTestAccount, handlers_ochrevector.DataSourceRecord{
		ID: "ds-b", KnowledgeBaseID: "kb-1",
		Source: handlers_ochrevector.SourceSpec{Bucket: "bucket-b", Prefix: "b/"},
	}))

	// job-1 really belongs to ds-a (matching Bucket+Prefix), but the request
	// below addresses it through ds-b's path.
	vector := &fakeBedrockAgentVectorService{describeJobResp: handlers_ochrevector.DescribeJobResponse{
		Job: handlers_ochrevector.JobRecord{ID: "job-1", IndexID: "idx-1", Source: handlers_ochrevector.SourceSpec{Bucket: "bucket-a", Prefix: "a/"}},
	}}
	_, err := GetIngestionJob(ctx, bedrockAgentTestAccount, kb, ds, vector, &bedrockagent.GetIngestionJobInput{
		KnowledgeBaseId: aws.String("kb-1"), DataSourceId: aws.String("ds-b"), IngestionJobId: aws.String("job-1"),
	})
	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorResourceNotFoundException))

	// Addressed through its real data source, the same job resolves fine.
	out, err := GetIngestionJob(ctx, bedrockAgentTestAccount, kb, ds, vector, &bedrockagent.GetIngestionJobInput{
		KnowledgeBaseId: aws.String("kb-1"), DataSourceId: aws.String("ds-a"), IngestionJobId: aws.String("job-1"),
	})
	require.NoError(t, err)
	assert.Equal(t, "job-1", aws.StringValue(out.IngestionJob.IngestionJobId))
}

func TestListIngestionJobs_FiltersToBoundIndexAndDataSourceSourceSpec(t *testing.T) {
	kb, ds := newBedrockAgentTestStores(t)
	ctx := context.Background()
	require.NoError(t, kb.Create(ctx, bedrockAgentTestAccount, handlers_ochrevector.KBRecord{ID: "kb-1", IndexID: "idx-1"}))
	require.NoError(t, ds.Create(ctx, bedrockAgentTestAccount, handlers_ochrevector.DataSourceRecord{
		ID: "ds-1", KnowledgeBaseID: "kb-1",
		Source: handlers_ochrevector.SourceSpec{Bucket: "b1", Prefix: "p1"},
	}))

	now := time.Now().UTC()
	vector := &fakeBedrockAgentVectorService{listJobsResp: handlers_ochrevector.ListJobsResponse{Jobs: []handlers_ochrevector.JobRecord{
		{ID: "job-match", IndexID: "idx-1", Source: handlers_ochrevector.SourceSpec{Bucket: "b1", Prefix: "p1"}, State: handlers_ochrevector.JobStateReady, CreatedAt: now, UpdatedAt: now},
		{ID: "job-wrong-index", IndexID: "idx-2", Source: handlers_ochrevector.SourceSpec{Bucket: "b1", Prefix: "p1"}, CreatedAt: now, UpdatedAt: now},
		{ID: "job-wrong-source", IndexID: "idx-1", Source: handlers_ochrevector.SourceSpec{Bucket: "other"}, CreatedAt: now, UpdatedAt: now},
	}}}

	out, err := ListIngestionJobs(ctx, bedrockAgentTestAccount, kb, ds, vector, &bedrockagent.ListIngestionJobsInput{
		KnowledgeBaseId: aws.String("kb-1"), DataSourceId: aws.String("ds-1"),
	})
	require.NoError(t, err)
	require.Len(t, out.IngestionJobSummaries, 1)
	assert.Equal(t, "job-match", aws.StringValue(out.IngestionJobSummaries[0].IngestionJobId))
	assert.Equal(t, bedrockagent.IngestionJobStatusComplete, aws.StringValue(out.IngestionJobSummaries[0].Status))
}
