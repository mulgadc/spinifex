// Exercises unexported ochre vector CLI command internals with no
// exported surface to drive them through.
//
//test:in-package
package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	handlers_ochrevector "github.com/mulgadc/spinifex/spinifex/handlers/ochrevector"
	"github.com/stretchr/testify/require"
)

// fakeVectorService serves scripted responses so the CLI's testable cores
// can be exercised without a daemon or a live NATS connection.
type fakeVectorService struct {
	createIndex    handlers_ochrevector.CreateIndexResponse
	createIndexErr error
	deleteIndexErr error
	list           []handlers_ochrevector.Record
	listErr        error
	ingest         handlers_ochrevector.IngestResponse
	ingestErr      error
	describeJob    handlers_ochrevector.JobRecord
	describeErr    error
	query          []handlers_ochrevector.QueryResult
	queryErr       error
	listJobs       []handlers_ochrevector.JobRecord
	listJobsErr    error

	createIndexInputs []*handlers_ochrevector.CreateIndexRequest
	deleteIndexInputs []*handlers_ochrevector.DeleteIndexRequest
	ingestInputs      []*handlers_ochrevector.IngestRequest
	queryInputs       []*handlers_ochrevector.QueryRequest
}

func (f *fakeVectorService) CreateIndex(_ context.Context, in *handlers_ochrevector.CreateIndexRequest, _ string) (*handlers_ochrevector.CreateIndexResponse, error) {
	f.createIndexInputs = append(f.createIndexInputs, in)
	if f.createIndexErr != nil {
		return nil, f.createIndexErr
	}
	out := f.createIndex
	if out.Index.ID == "" {
		out.Index.ID = in.IndexID
	}
	return &out, nil
}

func (f *fakeVectorService) DeleteIndex(_ context.Context, in *handlers_ochrevector.DeleteIndexRequest, _ string) (*handlers_ochrevector.DeleteIndexResponse, error) {
	f.deleteIndexInputs = append(f.deleteIndexInputs, in)
	if f.deleteIndexErr != nil {
		return nil, f.deleteIndexErr
	}
	return &handlers_ochrevector.DeleteIndexResponse{}, nil
}

func (f *fakeVectorService) ListIndexes(_ context.Context, _ *handlers_ochrevector.ListIndexesRequest, _ string) (*handlers_ochrevector.ListIndexesResponse, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return &handlers_ochrevector.ListIndexesResponse{Indexes: f.list}, nil
}

func (f *fakeVectorService) Ingest(_ context.Context, in *handlers_ochrevector.IngestRequest, _ string) (*handlers_ochrevector.IngestResponse, error) {
	f.ingestInputs = append(f.ingestInputs, in)
	if f.ingestErr != nil {
		return nil, f.ingestErr
	}
	return &f.ingest, nil
}

func (f *fakeVectorService) DescribeJob(_ context.Context, _ *handlers_ochrevector.DescribeJobRequest, _ string) (*handlers_ochrevector.DescribeJobResponse, error) {
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	return &handlers_ochrevector.DescribeJobResponse{Job: f.describeJob}, nil
}

func (f *fakeVectorService) Query(_ context.Context, in *handlers_ochrevector.QueryRequest, _ string) (*handlers_ochrevector.QueryResponse, error) {
	f.queryInputs = append(f.queryInputs, in)
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	return &handlers_ochrevector.QueryResponse{Results: f.query}, nil
}

func (f *fakeVectorService) ListJobs(_ context.Context, _ *handlers_ochrevector.ListJobsRequest, _ string) (*handlers_ochrevector.ListJobsResponse, error) {
	if f.listJobsErr != nil {
		return nil, f.listJobsErr
	}
	return &handlers_ochrevector.ListJobsResponse{Jobs: f.listJobs}, nil
}

var _ handlers_ochrevector.VectorService = (*fakeVectorService)(nil)

const testIndexID = "idx-0123456789abcdef0"

func TestRunIndexCreate_MintsIDAndPrintsRecord(t *testing.T) {
	svc := &fakeVectorService{createIndex: handlers_ochrevector.CreateIndexResponse{
		Index: handlers_ochrevector.Record{Name: "kb1", State: handlers_ochrevector.StateReady, Dimension: 768, EmbeddingModel: "nomic-embed-text-v1.5"},
	}}

	msg, err := runIndexCreate(context.Background(), svc, "kb1", 768, "nomic-embed-text-v1.5")
	require.NoError(t, err)
	require.Contains(t, msg, "READY")
	require.Contains(t, msg, "nomic-embed-text-v1.5")

	require.Len(t, svc.createIndexInputs, 1)
	require.NotEmpty(t, svc.createIndexInputs[0].IndexID, "the CLI must mint an index ID, not leave it empty")
	require.Equal(t, 768, svc.createIndexInputs[0].Dimension)
}

func TestRunIndexCreate_ErrorSurfaces(t *testing.T) {
	svc := &fakeVectorService{createIndexErr: errors.New("ValidationException")}
	_, err := runIndexCreate(context.Background(), svc, "kb1", 768, "m")
	require.ErrorContains(t, err, "ValidationException")
}

func TestRunIndexDelete_ReportsDeletion(t *testing.T) {
	svc := &fakeVectorService{}
	msg, err := runIndexDelete(context.Background(), svc, testIndexID)
	require.NoError(t, err)
	require.Contains(t, msg, testIndexID)
	require.Len(t, svc.deleteIndexInputs, 1)
	require.Equal(t, testIndexID, svc.deleteIndexInputs[0].IndexID)
}

func TestRunIndexDelete_ErrorSurfaces(t *testing.T) {
	svc := &fakeVectorService{deleteIndexErr: errors.New("nats: timeout")}
	_, err := runIndexDelete(context.Background(), svc, testIndexID)
	require.ErrorContains(t, err, "timeout")
}

func TestListIndexesOutput_NoIndexes(t *testing.T) {
	msg, err := listIndexesOutput(context.Background(), &fakeVectorService{})
	require.NoError(t, err)
	require.Equal(t, "No vector indexes.", msg)
}

func TestListIndexesOutput_ListsIndexes(t *testing.T) {
	svc := &fakeVectorService{list: []handlers_ochrevector.Record{
		{ID: testIndexID, Name: "kb1", State: handlers_ochrevector.StateReady, Dimension: 768, EmbeddingModel: "nomic-embed-text-v1.5"},
	}}
	msg, err := listIndexesOutput(context.Background(), svc)
	require.NoError(t, err)
	require.Contains(t, msg, testIndexID)
	require.Contains(t, msg, "READY")
}

func TestListIndexesOutput_ErrorSurfaces(t *testing.T) {
	svc := &fakeVectorService{listErr: errors.New("nats: no responders")}
	_, err := listIndexesOutput(context.Background(), svc)
	require.ErrorContains(t, err, "no responders")
}

func TestRunIngest_NoModelFlagSentByCLI(t *testing.T) {
	svc := &fakeVectorService{ingest: handlers_ochrevector.IngestResponse{
		Job: handlers_ochrevector.JobRecord{ID: "job-abc", IndexID: testIndexID, State: handlers_ochrevector.JobStatePending},
	}}

	msg, err := runIngest(context.Background(), svc, testIndexID, "docs", "kb/", 100, 10, map[string]string{"team": "eng"})
	require.NoError(t, err)
	require.Contains(t, msg, "job-abc")
	require.Contains(t, msg, "PENDING")

	require.Len(t, svc.ingestInputs, 1)
	in := svc.ingestInputs[0]
	require.Equal(t, testIndexID, in.IndexID)
	require.Equal(t, "docs", in.Source.Bucket)
	require.Equal(t, "kb/", in.Source.Prefix)
	require.Equal(t, 100, in.Source.ChunkSize)
	require.Equal(t, "eng", in.Source.Metadata["team"])
	// The CLI has no --model flag: the daemon stamps the index's pinned
	// model server-side, so the request's EmbeddingModel is left zero.
	require.Empty(t, in.Source.EmbeddingModel)
}

func TestRunIngest_ErrorSurfaces(t *testing.T) {
	svc := &fakeVectorService{ingestErr: errors.New("ResourceNotFoundException")}
	_, err := runIngest(context.Background(), svc, "no-such-index", "docs", "", 0, 0, nil)
	require.ErrorContains(t, err, "ResourceNotFoundException")
}

func TestRunJobDescribe_PrintsRecord(t *testing.T) {
	svc := &fakeVectorService{describeJob: handlers_ochrevector.JobRecord{
		ID: "job-abc", IndexID: testIndexID, State: handlers_ochrevector.JobStateReady, DocumentsDone: 3, DocumentsTotal: 3,
	}}
	out, err := runJobDescribe(context.Background(), svc, "job-abc")
	require.NoError(t, err)
	require.Contains(t, out, "job-abc")
	require.Contains(t, out, "3/3")
}

func TestRunJobDescribe_ErrorSurfaces(t *testing.T) {
	svc := &fakeVectorService{describeErr: errors.New("ochrevector: job not found")}
	_, err := runJobDescribe(context.Background(), svc, "no-such-job")
	require.ErrorContains(t, err, "job not found")
}

func TestRunQuery_TableTruncatesLongChunk(t *testing.T) {
	longChunk := strings.Repeat("x", cliChunkPreviewChars+50)
	svc := &fakeVectorService{query: []handlers_ochrevector.QueryResult{
		{Chunk: longChunk, SourceKey: "docs/a.txt", SourceOffset: 0, Score: 0.9},
	}}

	out, err := runQuery(context.Background(), svc, testIndexID, "hello", 5, "", false)
	require.NoError(t, err)
	require.Contains(t, out, "docs/a.txt")
	require.NotContains(t, out, longChunk, "the table must not print the full untruncated chunk")

	require.Len(t, svc.queryInputs, 1)
	require.Equal(t, testIndexID, svc.queryInputs[0].IndexID)
	require.Equal(t, "hello", svc.queryInputs[0].Text)
	require.Equal(t, 5, svc.queryInputs[0].K)
}

func TestRunQuery_JSONFlagPrintsFullChunk(t *testing.T) {
	longChunk := strings.Repeat("x", cliChunkPreviewChars+50)
	svc := &fakeVectorService{query: []handlers_ochrevector.QueryResult{
		{Chunk: longChunk, SourceKey: "docs/a.txt", Score: 0.9},
	}}

	out, err := runQuery(context.Background(), svc, testIndexID, "hello", 0, "", true)
	require.NoError(t, err)
	require.Contains(t, out, longChunk, "--json must print the full, untruncated chunk")
}

func TestRunQuery_NoResults(t *testing.T) {
	out, err := runQuery(context.Background(), &fakeVectorService{}, testIndexID, "hello", 0, "", false)
	require.NoError(t, err)
	require.Equal(t, "No results.", out)
}

func TestRunQuery_FilterFlagReachesRequest(t *testing.T) {
	svc := &fakeVectorService{}
	filterJSON := `{"Op":"equals","Key":"category","Value":"faq"}`

	_, err := runQuery(context.Background(), svc, testIndexID, "hello", 0, filterJSON, false)
	require.NoError(t, err)
	require.Len(t, svc.queryInputs, 1)
	require.NotNil(t, svc.queryInputs[0].Filter)
	require.Equal(t, handlers_ochrevector.FilterEquals, svc.queryInputs[0].Filter.Op)
	require.Equal(t, "category", svc.queryInputs[0].Filter.Key)
}

func TestRunQuery_InvalidFilterJSONErrors(t *testing.T) {
	_, err := runQuery(context.Background(), &fakeVectorService{}, testIndexID, "hello", 0, "{not json", false)
	require.Error(t, err)
}

func TestRunQuery_ErrorSurfaces(t *testing.T) {
	svc := &fakeVectorService{queryErr: errors.New("nats: timeout")}
	_, err := runQuery(context.Background(), svc, testIndexID, "hello", 0, "", false)
	require.ErrorContains(t, err, "timeout")
}

// withVectorService swaps vectorServiceFn for one returning svc, so a Run
// wrapper's real connect/validate/exit control flow runs without a daemon.
func withVectorService(t *testing.T, svc handlers_ochrevector.VectorService, connErr error) {
	t.Helper()
	orig := vectorServiceFn
	t.Cleanup(func() { vectorServiceFn = orig })
	vectorServiceFn = func() (handlers_ochrevector.VectorService, func(), error) {
		if connErr != nil {
			return nil, nil, connErr
		}
		return svc, func() {}, nil
	}
}

func TestRunOchreVectorIndexCreate_PrintsRecord(t *testing.T) {
	withVectorService(t, &fakeVectorService{createIndex: handlers_ochrevector.CreateIndexResponse{
		Index: handlers_ochrevector.Record{ID: testIndexID, State: handlers_ochrevector.StateReady},
	}}, nil)

	cmd := *ochreVectorIndexCreateCmd
	require.NoError(t, cmd.Flags().Set("name", "kb1"))
	require.NoError(t, cmd.Flags().Set("dimension", "768"))

	var out string
	code := withOchreExitCapture(t, func() {
		out = captureStdout(t, func() { runOchreVectorIndexCreate(&cmd, nil) })
	})
	require.Equal(t, -1, code)
	require.Contains(t, out, testIndexID)
}

func TestRunOchreVectorIndexCreate_ConnectFailureExits1(t *testing.T) {
	withVectorService(t, nil, errors.New("dial nats: connection refused"))

	cmd := *ochreVectorIndexCreateCmd
	require.NoError(t, cmd.Flags().Set("name", "kb1"))
	require.NoError(t, cmd.Flags().Set("dimension", "768"))

	code := withOchreExitCapture(t, func() { runOchreVectorIndexCreate(&cmd, nil) })
	require.Equal(t, 1, code)
}

func TestRunOchreVectorIndexDelete_ReportsDeletion(t *testing.T) {
	svc := &fakeVectorService{}
	withVectorService(t, svc, nil)

	var out string
	code := withOchreExitCapture(t, func() {
		out = captureStdout(t, func() { runOchreVectorIndexDelete(ochreVectorIndexDeleteCmd, []string{testIndexID}) })
	})
	require.Equal(t, -1, code)
	require.Contains(t, out, testIndexID)
	require.Len(t, svc.deleteIndexInputs, 1)
}

func TestRunOchreVectorIndexList_PrintsTable(t *testing.T) {
	withVectorService(t, &fakeVectorService{list: []handlers_ochrevector.Record{
		{ID: testIndexID, Name: "kb1", State: handlers_ochrevector.StateReady},
	}}, nil)

	var out string
	code := withOchreExitCapture(t, func() {
		out = captureStdout(t, func() { runOchreVectorIndexList(ochreVectorIndexListCmd, nil) })
	})
	require.Equal(t, -1, code)
	require.Contains(t, out, testIndexID)
}

func TestRunOchreVectorIngest_PrintsJobID(t *testing.T) {
	withVectorService(t, &fakeVectorService{ingest: handlers_ochrevector.IngestResponse{
		Job: handlers_ochrevector.JobRecord{ID: "job-abc", IndexID: testIndexID, State: handlers_ochrevector.JobStatePending},
	}}, nil)

	cmd := *ochreVectorIngestCmd
	require.NoError(t, cmd.Flags().Set("index", testIndexID))
	require.NoError(t, cmd.Flags().Set("bucket", "docs"))

	var out string
	code := withOchreExitCapture(t, func() {
		out = captureStdout(t, func() { runOchreVectorIngest(&cmd, nil) })
	})
	require.Equal(t, -1, code)
	require.Contains(t, out, "job-abc")
}

func TestRunOchreVectorJobDescribe_PrintsRecord(t *testing.T) {
	withVectorService(t, &fakeVectorService{describeJob: handlers_ochrevector.JobRecord{
		ID: "job-abc", IndexID: testIndexID, State: handlers_ochrevector.JobStateReady,
	}}, nil)

	var out string
	code := withOchreExitCapture(t, func() {
		out = captureStdout(t, func() { runOchreVectorJobDescribe(ochreVectorJobDescribeCmd, []string{"job-abc"}) })
	})
	require.Equal(t, -1, code)
	require.Contains(t, out, "job-abc")
}

func TestRunOchreVectorQuery_PrintsTable(t *testing.T) {
	withVectorService(t, &fakeVectorService{query: []handlers_ochrevector.QueryResult{
		{Chunk: "hello world", SourceKey: "docs/a.txt", Score: 0.9},
	}}, nil)

	cmd := *ochreVectorQueryCmd
	require.NoError(t, cmd.Flags().Set("index", testIndexID))
	require.NoError(t, cmd.Flags().Set("text", "hello"))

	var out string
	code := withOchreExitCapture(t, func() {
		out = captureStdout(t, func() { runOchreVectorQuery(&cmd, nil) })
	})
	require.Equal(t, -1, code)
	require.Contains(t, out, "docs/a.txt")
}

func TestRunOchreVectorQuery_ConnectFailureExits1(t *testing.T) {
	withVectorService(t, nil, errors.New("dial nats: connection refused"))

	cmd := *ochreVectorQueryCmd
	require.NoError(t, cmd.Flags().Set("index", testIndexID))
	require.NoError(t, cmd.Flags().Set("text", "hello"))

	code := withOchreExitCapture(t, func() { runOchreVectorQuery(&cmd, nil) })
	require.Equal(t, 1, code)
}
