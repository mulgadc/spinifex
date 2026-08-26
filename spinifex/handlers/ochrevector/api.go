package handlers_ochrevector

import (
	"context"
	"fmt"
	"time"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// NATS subjects the daemon subscribes and NATSVectorService forwards to,
// following the "eks.MethodName" naming EKS uses for its own internal
// (non-AWS-SDK-shaped) subjects.
const (
	SubjectCreateIndex = "ochre.vector.createIndex"
	SubjectDeleteIndex = "ochre.vector.deleteIndex"
	SubjectListIndexes = "ochre.vector.listIndexes"
	SubjectIngest      = "ochre.vector.ingest"
	SubjectDescribeJob = "ochre.vector.describeJob"
	SubjectQuery       = "ochre.vector.query"
	SubjectListJobs    = "ochre.vector.listJobs"
	SubjectStopJob     = "ochre.vector.stopJob"
)

// CreateIndexRequest names a new index and its fixed, per-index properties
// (D6): Dimension and EmbeddingModel are stamped once at create and never
// change for the index's lifetime.
type CreateIndexRequest struct {
	IndexID        string `json:"indexId"`
	Name           string `json:"name"`
	Dimension      int    `json:"dimension"`
	EmbeddingModel string `json:"embeddingModel"`
}

type CreateIndexResponse struct {
	Index Record `json:"index"`
}

// DeleteIndexRequest identifies the index to drop. Idempotent: deleting an
// already-absent index is a no-op success (Service.DeleteIndex).
type DeleteIndexRequest struct {
	IndexID string `json:"indexId"`
}

type DeleteIndexResponse struct{}

// ListIndexesRequest has no fields: accountID comes from the caller context
// (D10), never the payload.
type ListIndexesRequest struct{}

type ListIndexesResponse struct {
	Indexes []Record `json:"indexes"`
}

// IngestRequest starts an ingestion job against IndexID from Source. Source's
// EmbeddingModel/Dimension are ignored and overwritten server-side from the
// index's own registered values (see vectorService.Ingest) -- an index's
// embedding model is a property of the index (D6/D8), not of any one caller.
// DataSourceID is optional: the bedrock-agent gateway stamps it from the
// DataSource record driving this ingest, so the resulting job can later be
// tied back to it exactly; a direct ochre.vector.ingest caller may leave it
// empty.
type IngestRequest struct {
	IndexID      string     `json:"indexId"`
	Source       SourceSpec `json:"source"`
	DataSourceID string     `json:"dataSourceId,omitempty"`
}

type IngestResponse struct {
	Job JobRecord `json:"job"`
}

// DescribeJobRequest looks up one ingestion job's current record.
type DescribeJobRequest struct {
	JobID string `json:"jobId"`
}

type DescribeJobResponse struct {
	Job JobRecord `json:"job"`
}

// QueryRequest asks for the K nearest chunks to Text within IndexID, after
// Filter narrows the candidate set (D9). Text is embedded server-side against
// IndexID's own pinned model (D8) -- the request never carries a vector.
// K<=0 takes the backend's default; any K above the hard cap is clamped, not
// rejected (D8/D10).
type QueryRequest struct {
	IndexID string  `json:"indexId"`
	Text    string  `json:"text"`
	K       int     `json:"k,omitempty"`
	Filter  *Filter `json:"filter,omitempty"`
}

type QueryResponse struct {
	Results []QueryResult `json:"results"`
}

// ListJobsRequest has no fields: accountID comes from the caller context
// (D10), never the payload, mirroring ListIndexesRequest.
type ListJobsRequest struct{}

type ListJobsResponse struct {
	Jobs []JobRecord `json:"jobs"`
}

// StopJobRequest asks to cancel one in-flight ingestion job.
type StopJobRequest struct {
	JobID string `json:"jobId"`
}

type StopJobResponse struct {
	Job JobRecord `json:"job"`
}

// VectorService is the tenant vector-store surface: index CRUD, ingestion,
// job status/listing/cancellation, and similarity query. Every method's
// accountID argument is supplied by the transport alone (a NATS message
// header for the daemon subscription, D10) -- no request type above carries
// an account field, so a spoofed payload account can never widen a caller's
// own scope.
type VectorService interface {
	CreateIndex(ctx context.Context, req *CreateIndexRequest, accountID string) (*CreateIndexResponse, error)
	DeleteIndex(ctx context.Context, req *DeleteIndexRequest, accountID string) (*DeleteIndexResponse, error)
	ListIndexes(ctx context.Context, req *ListIndexesRequest, accountID string) (*ListIndexesResponse, error)
	Ingest(ctx context.Context, req *IngestRequest, accountID string) (*IngestResponse, error)
	DescribeJob(ctx context.Context, req *DescribeJobRequest, accountID string) (*DescribeJobResponse, error)
	ListJobs(ctx context.Context, req *ListJobsRequest, accountID string) (*ListJobsResponse, error)
	Query(ctx context.Context, req *QueryRequest, accountID string) (*QueryResponse, error)
	StopJob(ctx context.Context, req *StopJobRequest, accountID string) (*StopJobResponse, error)
}

// indexService is the CreateIndex/DeleteIndex/ListIndexes surface vectorService
// delegates to; *Service satisfies it structurally.
type indexService interface {
	CreateIndex(ctx context.Context, accountID, indexID string, params CreateIndexParams) (*Record, error)
	DeleteIndex(ctx context.Context, accountID, indexID string) error
	ListIndexes(ctx context.Context, accountID string) ([]Record, error)
}

var _ indexService = (*Service)(nil)

// ingestStarter is the StartIngest/StopJob surface vectorService delegates
// to; *IngestService satisfies it structurally.
type ingestStarter interface {
	StartIngest(ctx context.Context, accountID, indexID string, source SourceSpec, dataSourceID string) (*JobRecord, error)
	StopJob(ctx context.Context, accountID, jobID string) (*JobRecord, error)
}

var _ ingestStarter = (*IngestService)(nil)

// jobGetter is the job-lookup/listing surface DescribeJob and ListJobs
// delegate to; *JobStore satisfies it structurally.
type jobGetter interface {
	Get(ctx context.Context, accountID, jobID string) (*JobRecord, error)
	List(ctx context.Context, accountID string) ([]JobRecord, error)
}

var _ jobGetter = (*JobStore)(nil)

// indexGetter is the single-record registry lookup Ingest and Query use to
// resolve an index's pinned embedding model/dimension (D6/D8); *Registry
// satisfies it structurally.
type indexGetter interface {
	Get(ctx context.Context, accountID, indexID string) (*Record, error)
}

var _ indexGetter = (*Registry)(nil)

// maxResponseChunkChars bounds a single QueryResult's Chunk text in the wire
// response (D10): NATS's 1MB default payload cap divided generously across
// maxQueryK results, with headroom for JSON and metadata overhead. The full
// chunk is always recoverable from the source object via SourceKey/
// SourceOffset, so truncating here loses no data.
const maxResponseChunkChars = 4000

// truncateChunkForResponse bounds chunk to maxResponseChunkChars runes,
// appending a marker so a caller can tell a truncated chunk from a complete
// one without re-fetching the source object to check.
func truncateChunkForResponse(chunk string) string {
	runes := []rune(chunk)
	if len(runes) <= maxResponseChunkChars {
		return chunk
	}
	return string(runes[:maxResponseChunkChars]) + " …[truncated; see SourceKey/SourceOffset]"
}

// vectorService is the daemon-side VectorService implementation: index CRUD
// delegates to indexService (the orchestrator owning the registry+backend
// create/delete transitions), ingest/describe to the ingestion job stores,
// and Query resolves the index's pinned embedding model via registry before
// embedding and searching directly against backend.
type vectorService struct {
	index    indexService
	ingest   ingestStarter
	jobs     jobGetter
	registry indexGetter
	backend  VectorBackend
	embedder Embedder
	// reranker is optional: nil leaves Query at plain KNN top-k, matching
	// every deployment that has not configured a rerank endpoint.
	reranker Reranker
}

var _ VectorService = (*vectorService)(nil)

// NewVectorService constructs the daemon-side VectorService over its
// dependencies. index/ingest/jobs/registry/backend/embedder are accepted as
// the minimal interfaces above, so a *Service/*IngestService/*JobStore/
// *Registry/VectorBackend/Embedder wired for the rest of the daemon can be
// passed directly. reranker is optional -- a nil reranker leaves Query at
// plain KNN top-k, so an existing caller passing nil is unaffected.
func NewVectorService(index indexService, ingest ingestStarter, jobs jobGetter, registry indexGetter, backend VectorBackend, embedder Embedder, reranker Reranker) VectorService {
	return &vectorService{index: index, ingest: ingest, jobs: jobs, registry: registry, backend: backend, embedder: embedder, reranker: reranker}
}

func (s *vectorService) CreateIndex(ctx context.Context, req *CreateIndexRequest, accountID string) (*CreateIndexResponse, error) {
	if req == nil || req.IndexID == "" {
		return nil, fmt.Errorf("ochrevector: create index requires an indexId")
	}
	rec, err := s.index.CreateIndex(ctx, accountID, req.IndexID, CreateIndexParams{
		Name:           req.Name,
		Dimension:      req.Dimension,
		EmbeddingModel: req.EmbeddingModel,
	})
	if err != nil {
		return nil, fmt.Errorf("ochrevector: create index %s: %w", req.IndexID, err)
	}
	return &CreateIndexResponse{Index: *rec}, nil
}

func (s *vectorService) DeleteIndex(ctx context.Context, req *DeleteIndexRequest, accountID string) (*DeleteIndexResponse, error) {
	if req == nil || req.IndexID == "" {
		return nil, fmt.Errorf("ochrevector: delete index requires an indexId")
	}
	if err := s.index.DeleteIndex(ctx, accountID, req.IndexID); err != nil {
		return nil, fmt.Errorf("ochrevector: delete index %s: %w", req.IndexID, err)
	}
	return &DeleteIndexResponse{}, nil
}

func (s *vectorService) ListIndexes(ctx context.Context, _ *ListIndexesRequest, accountID string) (*ListIndexesResponse, error) {
	recs, err := s.index.ListIndexes(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("ochrevector: list indexes: %w", err)
	}
	return &ListIndexesResponse{Indexes: recs}, nil
}

// Ingest stamps Source's EmbeddingModel/Dimension from the index's own
// registered values before starting the job, so the stored source-spec (and
// every vector it produces) always matches the index it belongs to,
// regardless of what a caller sent.
func (s *vectorService) Ingest(ctx context.Context, req *IngestRequest, accountID string) (*IngestResponse, error) {
	if req == nil || req.IndexID == "" {
		return nil, fmt.Errorf("ochrevector: ingest requires an indexId")
	}
	idx, err := s.registry.Get(ctx, accountID, req.IndexID)
	if err != nil {
		return nil, fmt.Errorf("ochrevector: ingest: lookup index %s: %w", req.IndexID, err)
	}
	if idx == nil {
		return nil, fmt.Errorf("ochrevector: ingest: %w", ErrIndexNotFound)
	}

	source := req.Source
	source.EmbeddingModel = idx.EmbeddingModel
	source.Dimension = idx.Dimension

	job, err := s.ingest.StartIngest(ctx, accountID, req.IndexID, source, req.DataSourceID)
	if err != nil {
		return nil, fmt.Errorf("ochrevector: ingest: %w", err)
	}
	return &IngestResponse{Job: *job}, nil
}

func (s *vectorService) DescribeJob(ctx context.Context, req *DescribeJobRequest, accountID string) (*DescribeJobResponse, error) {
	if req == nil || req.JobID == "" {
		return nil, fmt.Errorf("ochrevector: describe job requires a jobId")
	}
	job, err := s.jobs.Get(ctx, accountID, req.JobID)
	if err != nil {
		return nil, fmt.Errorf("ochrevector: describe job %s: %w", req.JobID, err)
	}
	if job == nil {
		return nil, ErrJobNotFound
	}
	return &DescribeJobResponse{Job: *job}, nil
}

// ListJobs returns every ingestion job record owned by accountID.
func (s *vectorService) ListJobs(ctx context.Context, _ *ListJobsRequest, accountID string) (*ListJobsResponse, error) {
	jobs, err := s.jobs.List(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("ochrevector: list jobs: %w", err)
	}
	return &ListJobsResponse{Jobs: jobs}, nil
}

// StopJob cancels req.JobID's in-flight run via ingest.StopJob.
func (s *vectorService) StopJob(ctx context.Context, req *StopJobRequest, accountID string) (*StopJobResponse, error) {
	if req == nil || req.JobID == "" {
		return nil, fmt.Errorf("ochrevector: stop job requires a jobId")
	}
	job, err := s.ingest.StopJob(ctx, accountID, req.JobID)
	if err != nil {
		return nil, fmt.Errorf("ochrevector: stop job %s: %w", req.JobID, err)
	}
	return &StopJobResponse{Job: *job}, nil
}

// Query resolves IndexID's pinned embedding model from the registry (D8),
// embeds Text against it, then runs the similarity search directly against
// backend, optionally over-fetching and reranking (rerankTopK). A non-READY
// index (CREATING/DELETING/STALE) returns empty results rather than an
// error (D4): it is not yet, or no longer, queryable.
func (s *vectorService) Query(ctx context.Context, req *QueryRequest, accountID string) (*QueryResponse, error) {
	if req == nil || req.IndexID == "" || req.Text == "" {
		return nil, fmt.Errorf("ochrevector: query requires an indexId and text")
	}
	idx, err := s.registry.Get(ctx, accountID, req.IndexID)
	if err != nil {
		return nil, fmt.Errorf("ochrevector: query: lookup index %s: %w", req.IndexID, err)
	}
	if idx == nil {
		return nil, fmt.Errorf("ochrevector: query: %w", ErrIndexNotFound)
	}
	if idx.State != StateReady {
		return &QueryResponse{}, nil
	}

	vectors, err := s.embedder.Embed(ctx, idx.EmbeddingModel, []string{req.Text})
	if err != nil {
		return nil, fmt.Errorf("ochrevector: query: embed query text: %w", err)
	}
	if len(vectors) != 1 {
		return nil, fmt.Errorf("ochrevector: query: embed returned %d vectors for 1 input", len(vectors))
	}

	finalK := clampQueryK(req.K)
	fetchK := finalK
	if s.reranker != nil {
		fetchK = rerankFetchK(finalK)
	}

	results, err := s.backend.Query(ctx, accountID, req.IndexID, vectors[0], fetchK, req.Filter)
	if err != nil {
		return nil, fmt.Errorf("ochrevector: query: %w", err)
	}
	results = rerankTopK(ctx, s.reranker, req.Text, results, finalK)
	for i := range results {
		results[i].Chunk = truncateChunkForResponse(results[i].Chunk)
	}
	return &QueryResponse{Results: results}, nil
}

// defaultVectorNATSTimeout bounds a request-reply round trip. Ingest itself
// only claims a job before replying (the run happens in the background), so
// this only needs to cover the claim/query latency, not a full ingestion run.
const defaultVectorNATSTimeout = 30 * time.Second

// NATSVectorService is the NATS-forwarding VectorService client: any caller
// off the daemon's own process (the CLI, or a future KB-API wrapper) uses
// this instead of importing this package's registry/backend dependencies.
type NATSVectorService struct {
	nc      *nats.Conn
	timeout time.Duration
}

var _ VectorService = (*NATSVectorService)(nil)

// NewNATSVectorService constructs a client bound to nc.
func NewNATSVectorService(nc *nats.Conn) *NATSVectorService {
	return &NATSVectorService{nc: nc, timeout: defaultVectorNATSTimeout}
}

func (c *NATSVectorService) CreateIndex(ctx context.Context, req *CreateIndexRequest, accountID string) (*CreateIndexResponse, error) {
	return utils.NATSRequest[CreateIndexResponse](ctx, c.nc, SubjectCreateIndex, req, c.timeout, accountID)
}

func (c *NATSVectorService) DeleteIndex(ctx context.Context, req *DeleteIndexRequest, accountID string) (*DeleteIndexResponse, error) {
	return utils.NATSRequest[DeleteIndexResponse](ctx, c.nc, SubjectDeleteIndex, req, c.timeout, accountID)
}

func (c *NATSVectorService) ListIndexes(ctx context.Context, req *ListIndexesRequest, accountID string) (*ListIndexesResponse, error) {
	return utils.NATSRequest[ListIndexesResponse](ctx, c.nc, SubjectListIndexes, req, c.timeout, accountID)
}

func (c *NATSVectorService) Ingest(ctx context.Context, req *IngestRequest, accountID string) (*IngestResponse, error) {
	return utils.NATSRequest[IngestResponse](ctx, c.nc, SubjectIngest, req, c.timeout, accountID)
}

func (c *NATSVectorService) DescribeJob(ctx context.Context, req *DescribeJobRequest, accountID string) (*DescribeJobResponse, error) {
	return utils.NATSRequest[DescribeJobResponse](ctx, c.nc, SubjectDescribeJob, req, c.timeout, accountID)
}

func (c *NATSVectorService) ListJobs(ctx context.Context, req *ListJobsRequest, accountID string) (*ListJobsResponse, error) {
	return utils.NATSRequest[ListJobsResponse](ctx, c.nc, SubjectListJobs, req, c.timeout, accountID)
}

func (c *NATSVectorService) Query(ctx context.Context, req *QueryRequest, accountID string) (*QueryResponse, error) {
	return utils.NATSRequest[QueryResponse](ctx, c.nc, SubjectQuery, req, c.timeout, accountID)
}

func (c *NATSVectorService) StopJob(ctx context.Context, req *StopJobRequest, accountID string) (*StopJobResponse, error) {
	return utils.NATSRequest[StopJobResponse](ctx, c.nc, SubjectStopJob, req, c.timeout, accountID)
}
