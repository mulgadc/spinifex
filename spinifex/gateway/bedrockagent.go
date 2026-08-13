package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrockagent"
	"github.com/google/uuid"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_bedrock "github.com/mulgadc/spinifex/spinifex/gateway/bedrock"
	handlers_ochrevector "github.com/mulgadc/spinifex/spinifex/handlers/ochrevector"
)

// bedrockAgentRoute maps one HTTP method + path regex to an AWS action and handler.
type bedrockAgentRoute struct {
	method  string
	pattern *regexp.Regexp
	action  string
	handler bedrockAgentRouteHandler
}

// bedrockAgentRouteHandler invokes a per-action bedrock-agent (control-plane)
// gateway function. params holds the regex capture groups, PathUnescape'd.
// kb/ds are gw.BedrockAgentKB / gw.BedrockAgentDataSources; vector is
// gw.BedrockAgentVector, the NATSVectorService forwarding client to .9's
// daemon-side VectorService.
type bedrockAgentRouteHandler func(ctx context.Context, accountID, region string, params []string, body []byte, kb *handlers_ochrevector.KBStore, ds *handlers_ochrevector.DataSourceStore, vector handlers_ochrevector.VectorService) (any, error)

// bedrockAgentRoutes is the dispatch table. Real AWS HTTP paths/methods
// (verified against the vendored aws-sdk-go bedrockagent request
// definitions), not invented ones. More-specific paths must precede
// less-specific ones with the same prefix so the regex matcher picks the
// deeper route first.
var bedrockAgentRoutes = []bedrockAgentRoute{
	{"PUT", regexp.MustCompile(`^/knowledgebases/$`), "CreateKnowledgeBase",
		func(ctx context.Context, acct, region string, p []string, b []byte, kb *handlers_ochrevector.KBStore, _ *handlers_ochrevector.DataSourceStore, vector handlers_ochrevector.VectorService) (any, error) {
			input := new(bedrockagent.CreateKnowledgeBaseInput)
			if len(b) > 0 {
				if err := json.Unmarshal(b, input); err != nil {
					return nil, errors.New(awserrors.ErrorValidationException)
				}
			}
			return CreateKnowledgeBase(ctx, acct, region, kb, vector, input)
		}},
	{"POST", regexp.MustCompile(`^/knowledgebases/$`), "ListKnowledgeBases",
		func(ctx context.Context, acct, region string, p []string, b []byte, kb *handlers_ochrevector.KBStore, _ *handlers_ochrevector.DataSourceStore, _ handlers_ochrevector.VectorService) (any, error) {
			return ListKnowledgeBases(ctx, acct, kb, new(bedrockagent.ListKnowledgeBasesInput))
		}},
	{"GET", regexp.MustCompile(`^/knowledgebases/([^/]+)$`), "GetKnowledgeBase",
		func(ctx context.Context, acct, region string, p []string, b []byte, kb *handlers_ochrevector.KBStore, _ *handlers_ochrevector.DataSourceStore, _ handlers_ochrevector.VectorService) (any, error) {
			return GetKnowledgeBase(ctx, acct, region, kb, &bedrockagent.GetKnowledgeBaseInput{KnowledgeBaseId: aws.String(p[0])})
		}},
	{"DELETE", regexp.MustCompile(`^/knowledgebases/([^/]+)$`), "DeleteKnowledgeBase",
		func(ctx context.Context, acct, region string, p []string, b []byte, kb *handlers_ochrevector.KBStore, ds *handlers_ochrevector.DataSourceStore, vector handlers_ochrevector.VectorService) (any, error) {
			return DeleteKnowledgeBase(ctx, acct, kb, ds, vector, &bedrockagent.DeleteKnowledgeBaseInput{KnowledgeBaseId: aws.String(p[0])})
		}},
	{"PUT", regexp.MustCompile(`^/knowledgebases/([^/]+)/datasources/$`), "CreateDataSource",
		func(ctx context.Context, acct, region string, p []string, b []byte, kb *handlers_ochrevector.KBStore, ds *handlers_ochrevector.DataSourceStore, _ handlers_ochrevector.VectorService) (any, error) {
			input := new(bedrockagent.CreateDataSourceInput)
			if len(b) > 0 {
				if err := json.Unmarshal(b, input); err != nil {
					return nil, errors.New(awserrors.ErrorValidationException)
				}
			}
			input.KnowledgeBaseId = aws.String(p[0])
			return CreateDataSource(ctx, acct, region, kb, ds, input)
		}},
	{"POST", regexp.MustCompile(`^/knowledgebases/([^/]+)/datasources/$`), "ListDataSources",
		func(ctx context.Context, acct, region string, p []string, b []byte, kb *handlers_ochrevector.KBStore, ds *handlers_ochrevector.DataSourceStore, _ handlers_ochrevector.VectorService) (any, error) {
			return ListDataSources(ctx, acct, kb, ds, &bedrockagent.ListDataSourcesInput{KnowledgeBaseId: aws.String(p[0])})
		}},
	{"GET", regexp.MustCompile(`^/knowledgebases/([^/]+)/datasources/([^/]+)$`), "GetDataSource",
		func(ctx context.Context, acct, region string, p []string, b []byte, _ *handlers_ochrevector.KBStore, ds *handlers_ochrevector.DataSourceStore, _ handlers_ochrevector.VectorService) (any, error) {
			return GetDataSource(ctx, acct, region, ds, &bedrockagent.GetDataSourceInput{KnowledgeBaseId: aws.String(p[0]), DataSourceId: aws.String(p[1])})
		}},
	{"DELETE", regexp.MustCompile(`^/knowledgebases/([^/]+)/datasources/([^/]+)$`), "DeleteDataSource",
		func(ctx context.Context, acct, region string, p []string, b []byte, _ *handlers_ochrevector.KBStore, ds *handlers_ochrevector.DataSourceStore, _ handlers_ochrevector.VectorService) (any, error) {
			return DeleteDataSource(ctx, acct, ds, &bedrockagent.DeleteDataSourceInput{KnowledgeBaseId: aws.String(p[0]), DataSourceId: aws.String(p[1])})
		}},
	{"PUT", regexp.MustCompile(`^/knowledgebases/([^/]+)/datasources/([^/]+)/ingestionjobs/$`), "StartIngestionJob",
		func(ctx context.Context, acct, region string, p []string, b []byte, kb *handlers_ochrevector.KBStore, ds *handlers_ochrevector.DataSourceStore, vector handlers_ochrevector.VectorService) (any, error) {
			return StartIngestionJob(ctx, acct, kb, ds, vector, &bedrockagent.StartIngestionJobInput{KnowledgeBaseId: aws.String(p[0]), DataSourceId: aws.String(p[1])})
		}},
	{"POST", regexp.MustCompile(`^/knowledgebases/([^/]+)/datasources/([^/]+)/ingestionjobs/$`), "ListIngestionJobs",
		func(ctx context.Context, acct, region string, p []string, b []byte, kb *handlers_ochrevector.KBStore, ds *handlers_ochrevector.DataSourceStore, vector handlers_ochrevector.VectorService) (any, error) {
			return ListIngestionJobs(ctx, acct, kb, ds, vector, &bedrockagent.ListIngestionJobsInput{KnowledgeBaseId: aws.String(p[0]), DataSourceId: aws.String(p[1])})
		}},
	{"GET", regexp.MustCompile(`^/knowledgebases/([^/]+)/datasources/([^/]+)/ingestionjobs/([^/]+)$`), "GetIngestionJob",
		func(ctx context.Context, acct, region string, p []string, b []byte, kb *handlers_ochrevector.KBStore, ds *handlers_ochrevector.DataSourceStore, vector handlers_ochrevector.VectorService) (any, error) {
			return GetIngestionJob(ctx, acct, kb, ds, vector, &bedrockagent.GetIngestionJobInput{KnowledgeBaseId: aws.String(p[0]), DataSourceId: aws.String(p[1]), IngestionJobId: aws.String(p[2])})
		}},
}

// lookupBedrockAgentAction matches method+path against bedrockAgentRoutes,
// returning the action, path params, and handler, or ("", nil, nil, false) on
// no match. path must be r.URL.EscapedPath(): captured params are
// PathUnescape'd before returning, mirroring lookupBedrockAction.
func lookupBedrockAgentAction(method, path string) (string, []string, bedrockAgentRouteHandler, bool) {
	for _, route := range bedrockAgentRoutes {
		if route.method != method {
			continue
		}
		m := route.pattern.FindStringSubmatch(path)
		if m == nil {
			continue
		}
		var params []string
		if len(m) > 1 {
			params = make([]string, 0, len(m)-1)
			for _, raw := range m[1:] {
				decoded, err := url.PathUnescape(raw)
				if err != nil {
					slog.Debug("bedrock-agent: bad percent-encoding in path param", "param", raw, "err", err)
					decoded = raw
				}
				params = append(params, decoded)
			}
		}
		return route.action, params, route.handler, true
	}
	return "", nil, nil, false
}

// BedrockAgent_Request dispatches bedrock-agent (control-plane) REST-JSON
// requests: resolves method+path to an action, reads the body, calls the
// handler, and serialises the output as JSON, mirroring Bedrock_Request.
func (gw *GatewayConfig) BedrockAgent_Request(w http.ResponseWriter, r *http.Request) error {
	action, params, handler, ok := lookupBedrockAgentAction(r.Method, r.URL.EscapedPath())
	if !ok {
		slog.DebugContext(r.Context(), "bedrock-agent: no route for request", "method", r.Method, "path", r.URL.Path)
		return errors.New(awserrors.ErrorInvalidAction)
	}

	if err := gw.checkPolicy(r, "bedrock-agent", action); err != nil {
		return err
	}

	if gw.BedrockAgentVector == nil || gw.BedrockAgentKB == nil || gw.BedrockAgentDataSources == nil {
		return errors.New(awserrors.ErrorServerInternal)
	}

	accountID, _ := r.Context().Value(ctxAccountID).(string)
	if accountID == "" {
		slog.ErrorContext(r.Context(), "BedrockAgent_Request: no account ID in auth context")
		return errors.New(awserrors.ErrorServerInternal)
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.ErrorContext(r.Context(), "BedrockAgent_Request: failed to read body", "err", err)
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	output, err := handler(r.Context(), accountID, gw.Region, params, body, gw.BedrockAgentKB, gw.BedrockAgentDataSources, gw.BedrockAgentVector)
	if err != nil {
		return err
	}

	gateway_bedrock.WriteJSONResponse(w, output)
	return nil
}

// bedrockAgentKnowledgeBaseResourceType is the ARN resource-type segment a
// knowledge base renders under. DataSource carries no ARN field in AWS's own
// shape (only KnowledgeBase does), so there is no data-source counterpart.
const bedrockAgentKnowledgeBaseResourceType = "knowledge-base"

// formatKnowledgeBaseARN builds the ARN a CreateKnowledgeBase call returns,
// mirroring gateway_bedrock.FormatGuardrailARN's shape.
func formatKnowledgeBaseARN(region, accountID, id string) string {
	return fmt.Sprintf("arn:aws:bedrock:%s:%s:%s/%s", region, accountID, bedrockAgentKnowledgeBaseResourceType, id)
}

// nonEmptyStringPtr returns nil for an empty string, so an optional field
// AWS omits when unset stays omitted here too, mirroring
// gateway_bedrock.nonEmptyStringPtr (unexported there, so not reusable).
func nonEmptyStringPtr(s string) *string {
	if s == "" {
		return nil
	}
	return aws.String(s)
}

// embeddingModelIDFromARN extracts the bare foundation-model id from an
// embeddingModelArn ("arn:aws:bedrock:region::foundation-model/id" ->
// "id"), the identifier .9's CreateIndexRequest.EmbeddingModel and its
// embedder key on. An arn that is not shaped that way is used verbatim, so a
// caller-supplied plain model id still works.
func embeddingModelIDFromARN(arn string) string {
	const marker = "foundation-model/"
	if idx := strings.LastIndex(arn, marker); idx != -1 {
		return arn[idx+len(marker):]
	}
	return arn
}

// bucketNameFromS3ARN extracts the bucket name from an S3 bucket ARN
// ("arn:aws:s3:::bucket-name" -> "bucket-name"). A value that is not shaped
// like an ARN is used verbatim, so a bare bucket name still works.
func bucketNameFromS3ARN(arn string) string {
	const prefix = "arn:aws:s3:::"
	if after, ok := strings.CutPrefix(arn, prefix); ok {
		return after
	}
	return arn
}

// formatS3BucketARN is bucketNameFromS3ARN's inverse, used to render
// SourceSpec.Bucket back into the AWS bucketArn shape on read.
func formatS3BucketARN(bucket string) string {
	return "arn:aws:s3:::" + bucket
}

// kbStatusToAWS translates .9's internal index lifecycle state (registry.go)
// to AWS's KnowledgeBaseStatus wire values. CreateKnowledgeBase is
// synchronous end-to-end (Service.CreateIndex blocks until READY or rolls
// back on error, like CreateGuardrail's own no-async-transition contract),
// so only StateReady is ever actually persisted here in Pass 1.
func kbStatusToAWS(status string) string {
	switch status {
	case handlers_ochrevector.StateReady:
		return bedrockagent.KnowledgeBaseStatusActive
	case handlers_ochrevector.StateCreating:
		return bedrockagent.KnowledgeBaseStatusCreating
	case handlers_ochrevector.StateDeleting:
		return bedrockagent.KnowledgeBaseStatusDeleting
	default:
		return bedrockagent.KnowledgeBaseStatusFailed
	}
}

// dataSourceStatusToAWS is kbStatusToAWS's sibling for DataSourceStatus.
func dataSourceStatusToAWS(status string) string {
	switch status {
	case handlers_ochrevector.StateDeleting:
		return bedrockagent.DataSourceStatusDeleting
	default:
		return bedrockagent.DataSourceStatusAvailable
	}
}

// jobStateToAWS translates .9's JobRecord.State to AWS's IngestionJobStatus
// wire values.
func jobStateToAWS(state string) string {
	switch state {
	case handlers_ochrevector.JobStatePending:
		return bedrockagent.IngestionJobStatusStarting
	case handlers_ochrevector.JobStateRunning:
		return bedrockagent.IngestionJobStatusInProgress
	case handlers_ochrevector.JobStateReady:
		return bedrockagent.IngestionJobStatusComplete
	default:
		return bedrockagent.IngestionJobStatusFailed
	}
}

// errKBNotFound reports a knowledge-base-specific ResourceNotFoundException.
func errKBNotFound(id string) error {
	return awserrors.Errorf(awserrors.ErrorResourceNotFoundException, "knowledge base %q not found", id)
}

// errDataSourceNotFound reports a data-source-specific ResourceNotFoundException.
func errDataSourceNotFound(id string) error {
	return awserrors.Errorf(awserrors.ErrorResourceNotFoundException, "data source %q not found", id)
}

// errIngestionJobNotFound reports an ingestion-job-specific ResourceNotFoundException.
func errIngestionJobNotFound(id string) error {
	return awserrors.Errorf(awserrors.ErrorResourceNotFoundException, "ingestion job %q not found", id)
}

// translateVectorErr maps .9's ochrevector domain errors onto AWS error
// codes; any other error (a transport failure, an unwrapped internal error)
// passes through unchanged so ErrorHandler's ResolveErrorDetail sanitizes it
// to ServerInternal rather than mis-reporting it as a client error.
func translateVectorErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, handlers_ochrevector.ErrIndexNotFound), errors.Is(err, handlers_ochrevector.ErrJobNotFound):
		return errors.New(awserrors.ErrorResourceNotFoundException)
	case errors.Is(err, handlers_ochrevector.ErrIndexExists):
		return errors.New(awserrors.ErrorConflictException)
	default:
		return err
	}
}

// kbRecordToOutput builds the AWS KnowledgeBase shape from rec. The stored
// StorageConfigJSON blob is decoded back into the typed AWS struct so
// Get/List echo exactly what CreateKnowledgeBase accepted (D5: accepted and
// stubbed, never acted on).
func kbRecordToOutput(region, accountID string, rec handlers_ochrevector.KBRecord) (*bedrockagent.KnowledgeBase, error) {
	storageConfig := new(bedrockagent.StorageConfiguration)
	if len(rec.StorageConfigJSON) > 0 {
		if err := json.Unmarshal(rec.StorageConfigJSON, storageConfig); err != nil {
			return nil, fmt.Errorf("bedrock-agent: decode stored storage configuration for %s: %w", rec.ID, err)
		}
	}
	embeddingModelArn := rec.EmbeddingModelArn
	if embeddingModelArn == "" {
		embeddingModelArn = rec.EmbeddingModel
	}
	return &bedrockagent.KnowledgeBase{
		CreatedAt:        aws.Time(rec.CreatedAt),
		Description:      nonEmptyStringPtr(rec.Description),
		KnowledgeBaseArn: aws.String(formatKnowledgeBaseARN(region, accountID, rec.ID)),
		KnowledgeBaseConfiguration: &bedrockagent.KnowledgeBaseConfiguration{
			Type: aws.String(bedrockagent.KnowledgeBaseTypeVector),
			VectorKnowledgeBaseConfiguration: &bedrockagent.VectorKnowledgeBaseConfiguration{
				EmbeddingModelArn: aws.String(embeddingModelArn),
				EmbeddingModelConfiguration: &bedrockagent.EmbeddingModelConfiguration{
					BedrockEmbeddingModelConfiguration: &bedrockagent.BedrockEmbeddingModelConfiguration{
						Dimensions: aws.Int64(int64(rec.Dimension)),
					},
				},
			},
		},
		KnowledgeBaseId:      aws.String(rec.ID),
		Name:                 aws.String(rec.Name),
		RoleArn:              aws.String(rec.RoleArn),
		Status:               aws.String(kbStatusToAWS(rec.Status)),
		StorageConfiguration: storageConfig,
		UpdatedAt:            aws.Time(rec.UpdatedAt),
	}, nil
}

// CreateKnowledgeBase maps CreateKnowledgeBaseInput onto
// VectorService.CreateIndex (D1: one KB binds exactly one index,
// embeddingModelArn -> EmbeddingModel+Dimension) and persists a KBRecord.
// storageConfiguration and roleArn are accepted and stored verbatim for
// round-trip echo but never honored (D5). A KBStore.Create collision (id
// reuse) rolls the just-created index back, so no orphan index survives a
// failed claim.
func CreateKnowledgeBase(ctx context.Context, accountID, region string, kb *handlers_ochrevector.KBStore, vector handlers_ochrevector.VectorService, input *bedrockagent.CreateKnowledgeBaseInput) (*bedrockagent.CreateKnowledgeBaseOutput, error) {
	if input == nil || aws.StringValue(input.Name) == "" || aws.StringValue(input.RoleArn) == "" ||
		input.StorageConfiguration == nil || input.KnowledgeBaseConfiguration == nil {
		return nil, errors.New(awserrors.ErrorValidationException)
	}
	kbConfig := input.KnowledgeBaseConfiguration
	if aws.StringValue(kbConfig.Type) != bedrockagent.KnowledgeBaseTypeVector || kbConfig.VectorKnowledgeBaseConfiguration == nil {
		return nil, awserrors.Errorf(awserrors.ErrorValidationException, "bedrock-agent: only VECTOR knowledge bases are supported")
	}
	vecConfig := kbConfig.VectorKnowledgeBaseConfiguration
	embeddingModelArn := aws.StringValue(vecConfig.EmbeddingModelArn)
	if embeddingModelArn == "" {
		return nil, errors.New(awserrors.ErrorValidationException)
	}
	embeddingModel := embeddingModelIDFromARN(embeddingModelArn)

	dimension := 0
	if vecConfig.EmbeddingModelConfiguration != nil && vecConfig.EmbeddingModelConfiguration.BedrockEmbeddingModelConfiguration != nil {
		dimension = int(aws.Int64Value(vecConfig.EmbeddingModelConfiguration.BedrockEmbeddingModelConfiguration.Dimensions))
	}
	if dimension <= 0 {
		return nil, awserrors.Errorf(awserrors.ErrorValidationException,
			"bedrock-agent: embeddingModelConfiguration.bedrockEmbeddingModelConfiguration.dimensions is required")
	}

	id := uuid.NewString()
	indexResp, err := vector.CreateIndex(ctx, &handlers_ochrevector.CreateIndexRequest{
		IndexID:        id,
		Name:           aws.StringValue(input.Name),
		Dimension:      dimension,
		EmbeddingModel: embeddingModel,
	}, accountID)
	if err != nil {
		return nil, translateVectorErr(err)
	}

	storageJSON, err := json.Marshal(input.StorageConfiguration)
	if err != nil {
		return nil, fmt.Errorf("bedrock-agent: encode storage configuration: %w", err)
	}

	now := time.Now().UTC()
	rec := handlers_ochrevector.KBRecord{
		ID:                id,
		Name:              aws.StringValue(input.Name),
		Description:       aws.StringValue(input.Description),
		Status:            handlers_ochrevector.StateReady,
		EmbeddingModel:    embeddingModel,
		EmbeddingModelArn: embeddingModelArn,
		Dimension:         dimension,
		IndexID:           indexResp.Index.ID,
		RoleArn:           aws.StringValue(input.RoleArn),
		StorageConfigJSON: storageJSON,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := kb.Create(ctx, accountID, rec); err != nil {
		// The KB claim failed, but the index it would have bound to already
		// exists: kb.Create's error is still the operation result returned
		// below, so a rollback failure here must not be swallowed -- log it
		// at Error with the orphaned index id so it stays observable even
		// though the caller never sees it.
		if _, delErr := vector.DeleteIndex(ctx, &handlers_ochrevector.DeleteIndexRequest{IndexID: id}, accountID); delErr != nil {
			slog.ErrorContext(ctx, "bedrock-agent: rollback delete index after knowledge base claim failure left an orphaned index", "index", id, "err", delErr)
		}
		if errors.Is(err, handlers_ochrevector.ErrKBExists) {
			return nil, errors.New(awserrors.ErrorConflictException)
		}
		return nil, err
	}

	out, err := kbRecordToOutput(region, accountID, rec)
	if err != nil {
		return nil, err
	}
	return &bedrockagent.CreateKnowledgeBaseOutput{KnowledgeBase: out}, nil
}

// GetKnowledgeBase looks up id in kb, returning ResourceNotFoundException for
// a foreign account or an unknown id alike so a caller cannot distinguish
// "not yours" from "does not exist".
func GetKnowledgeBase(ctx context.Context, accountID, region string, kb *handlers_ochrevector.KBStore, input *bedrockagent.GetKnowledgeBaseInput) (*bedrockagent.GetKnowledgeBaseOutput, error) {
	if input == nil || aws.StringValue(input.KnowledgeBaseId) == "" {
		return nil, errors.New(awserrors.ErrorValidationException)
	}
	id := aws.StringValue(input.KnowledgeBaseId)
	rec, err := kb.Get(ctx, accountID, id)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, errKBNotFound(id)
	}
	out, err := kbRecordToOutput(region, accountID, *rec)
	if err != nil {
		return nil, err
	}
	return &bedrockagent.GetKnowledgeBaseOutput{KnowledgeBase: out}, nil
}

// ListKnowledgeBases returns the caller account's own knowledge bases,
// sorted by creation time (id as a deterministic tie-breaker), mirroring
// gateway_bedrock.ListGuardrails.
func ListKnowledgeBases(ctx context.Context, accountID string, kb *handlers_ochrevector.KBStore, _ *bedrockagent.ListKnowledgeBasesInput) (*bedrockagent.ListKnowledgeBasesOutput, error) {
	recs, err := kb.List(ctx, accountID)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(recs, func(a, b handlers_ochrevector.KBRecord) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	summaries := make([]*bedrockagent.KnowledgeBaseSummary, 0, len(recs))
	for _, rec := range recs {
		summaries = append(summaries, &bedrockagent.KnowledgeBaseSummary{
			Description:     nonEmptyStringPtr(rec.Description),
			KnowledgeBaseId: aws.String(rec.ID),
			Name:            aws.String(rec.Name),
			Status:          aws.String(kbStatusToAWS(rec.Status)),
			UpdatedAt:       aws.Time(rec.UpdatedAt),
		})
	}
	return &bedrockagent.ListKnowledgeBasesOutput{KnowledgeBaseSummaries: summaries}, nil
}

// DeleteKnowledgeBase drops id's bound index, cascades to every data source
// under it (D-arch: the gateway owns bedrock-agent resource metadata, so the
// cascade lives here rather than in a daemon-side operator), then removes the
// KB record itself. Not idempotent on a foreign/unknown id (reports
// ResourceNotFoundException), unlike gateway_bedrock.DeleteGuardrail's
// idempotent-delete contract, because AWS's own DeleteKnowledgeBase does the
// same.
func DeleteKnowledgeBase(ctx context.Context, accountID string, kb *handlers_ochrevector.KBStore, ds *handlers_ochrevector.DataSourceStore, vector handlers_ochrevector.VectorService, input *bedrockagent.DeleteKnowledgeBaseInput) (*bedrockagent.DeleteKnowledgeBaseOutput, error) {
	if input == nil || aws.StringValue(input.KnowledgeBaseId) == "" {
		return nil, errors.New(awserrors.ErrorValidationException)
	}
	id := aws.StringValue(input.KnowledgeBaseId)
	rec, err := kb.Get(ctx, accountID, id)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, errKBNotFound(id)
	}

	sources, err := ds.ListByKnowledgeBase(ctx, accountID, id)
	if err != nil {
		return nil, err
	}
	for _, src := range sources {
		if err := ds.Delete(ctx, accountID, src.ID); err != nil {
			return nil, fmt.Errorf("bedrock-agent: delete cascaded data source %s: %w", src.ID, err)
		}
	}

	// A missing bound index is treated as already deleted, not a failure:
	// the KB record must not be left dangling with no way to retry the
	// delete just because its index is already gone (Delete is idempotent
	// w.r.t. the index). Any other error still aborts before the record
	// itself is removed.
	if _, err := vector.DeleteIndex(ctx, &handlers_ochrevector.DeleteIndexRequest{IndexID: rec.IndexID}, accountID); err != nil && !errors.Is(err, handlers_ochrevector.ErrIndexNotFound) {
		return nil, translateVectorErr(err)
	}
	if err := kb.Delete(ctx, accountID, id); err != nil {
		return nil, err
	}
	return &bedrockagent.DeleteKnowledgeBaseOutput{
		KnowledgeBaseId: aws.String(id),
		Status:          aws.String(bedrockagent.KnowledgeBaseStatusDeleting),
	}, nil
}

// dataSourceRecordToOutput builds the AWS DataSource shape from rec. Only S3
// data sources are supported (CreateDataSource rejects any other type), so
// this always renders an S3Configuration.
func dataSourceRecordToOutput(rec handlers_ochrevector.DataSourceRecord) *bedrockagent.DataSource {
	var inclusionPrefixes []*string
	if rec.Source.Prefix != "" {
		inclusionPrefixes = []*string{aws.String(rec.Source.Prefix)}
	}
	return &bedrockagent.DataSource{
		CreatedAt: aws.Time(rec.CreatedAt),
		DataSourceConfiguration: &bedrockagent.DataSourceConfiguration{
			Type: aws.String(bedrockagent.DataSourceTypeS3),
			S3Configuration: &bedrockagent.S3DataSourceConfiguration{
				BucketArn:         aws.String(formatS3BucketARN(rec.Source.Bucket)),
				InclusionPrefixes: inclusionPrefixes,
			},
		},
		DataSourceId:    aws.String(rec.ID),
		Description:     nonEmptyStringPtr(rec.Description),
		KnowledgeBaseId: aws.String(rec.KnowledgeBaseID),
		Name:            aws.String(rec.Name),
		Status:          aws.String(dataSourceStatusToAWS(rec.Status)),
		UpdatedAt:       aws.Time(rec.UpdatedAt),
	}
}

// CreateDataSource maps CreateDataSourceInput's S3 + chunking config onto a
// SourceSpec and persists a DataSourceRecord under kbID. Only S3 data
// sources are supported (WEB/CONFLUENCE/SALESFORCE/SHAREPOINT are not
// stubbed -- there is no engine behind them to accept-and-ignore against, so
// they are rejected rather than silently accepted and never ingested).
// ChunkSize/ChunkOverlap are left zero when fixedSizeChunkingConfiguration is
// omitted, so .9's own ingest path applies DefaultChunkSize/
// DefaultChunkOverlap (D3). Data-source-level Metadata is left empty: AWS's
// CreateDataSourceInput carries no arbitrary metadata map to source it from
// (real per-document metadata comes from S3 .metadata.json sidecar files at
// ingest time, out of scope for Pass 1).
func CreateDataSource(ctx context.Context, accountID, region string, kb *handlers_ochrevector.KBStore, ds *handlers_ochrevector.DataSourceStore, input *bedrockagent.CreateDataSourceInput) (*bedrockagent.CreateDataSourceOutput, error) {
	if input == nil || aws.StringValue(input.KnowledgeBaseId) == "" || aws.StringValue(input.Name) == "" || input.DataSourceConfiguration == nil {
		return nil, errors.New(awserrors.ErrorValidationException)
	}
	kbID := aws.StringValue(input.KnowledgeBaseId)
	kbRec, err := kb.Get(ctx, accountID, kbID)
	if err != nil {
		return nil, err
	}
	if kbRec == nil {
		return nil, errKBNotFound(kbID)
	}

	dsConfig := input.DataSourceConfiguration
	if aws.StringValue(dsConfig.Type) != bedrockagent.DataSourceTypeS3 || dsConfig.S3Configuration == nil {
		return nil, awserrors.Errorf(awserrors.ErrorValidationException, "bedrock-agent: only S3 data sources are supported")
	}
	s3Config := dsConfig.S3Configuration
	bucketArn := aws.StringValue(s3Config.BucketArn)
	if bucketArn == "" {
		return nil, errors.New(awserrors.ErrorValidationException)
	}
	prefix := ""
	if len(s3Config.InclusionPrefixes) > 0 {
		prefix = aws.StringValue(s3Config.InclusionPrefixes[0])
	}

	var chunkSize, chunkOverlap int
	if input.VectorIngestionConfiguration != nil && input.VectorIngestionConfiguration.ChunkingConfiguration != nil {
		cc := input.VectorIngestionConfiguration.ChunkingConfiguration
		if aws.StringValue(cc.ChunkingStrategy) == bedrockagent.ChunkingStrategyFixedSize && cc.FixedSizeChunkingConfiguration != nil {
			maxTokens := aws.Int64Value(cc.FixedSizeChunkingConfiguration.MaxTokens)
			overlapPct := aws.Int64Value(cc.FixedSizeChunkingConfiguration.OverlapPercentage)
			chunkSize = int(maxTokens)
			// D3: ChunkOverlap = round(maxTokens * overlapPercentage/100).
			chunkOverlap = int(math.Round(float64(maxTokens) * float64(overlapPct) / 100))
		}
	}

	id := uuid.NewString()
	now := time.Now().UTC()
	rec := handlers_ochrevector.DataSourceRecord{
		ID:              id,
		KnowledgeBaseID: kbID,
		Name:            aws.StringValue(input.Name),
		Description:     aws.StringValue(input.Description),
		Status:          handlers_ochrevector.StateReady,
		Source: handlers_ochrevector.SourceSpec{
			Bucket:         bucketNameFromS3ARN(bucketArn),
			Prefix:         prefix,
			ChunkSize:      chunkSize,
			ChunkOverlap:   chunkOverlap,
			EmbeddingModel: kbRec.EmbeddingModel,
			Dimension:      kbRec.Dimension,
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := ds.Create(ctx, accountID, rec); err != nil {
		if errors.Is(err, handlers_ochrevector.ErrDataSourceExists) {
			return nil, errors.New(awserrors.ErrorConflictException)
		}
		return nil, err
	}
	return &bedrockagent.CreateDataSourceOutput{DataSource: dataSourceRecordToOutput(rec)}, nil
}

// GetDataSource looks up dataSourceId scoped to knowledgeBaseId: a data
// source that exists but belongs to a different knowledge base reports
// ResourceNotFoundException, the same as one that does not exist at all.
func GetDataSource(ctx context.Context, accountID, region string, ds *handlers_ochrevector.DataSourceStore, input *bedrockagent.GetDataSourceInput) (*bedrockagent.GetDataSourceOutput, error) {
	if input == nil || aws.StringValue(input.KnowledgeBaseId) == "" || aws.StringValue(input.DataSourceId) == "" {
		return nil, errors.New(awserrors.ErrorValidationException)
	}
	id := aws.StringValue(input.DataSourceId)
	rec, err := ds.Get(ctx, accountID, id)
	if err != nil {
		return nil, err
	}
	if rec == nil || rec.KnowledgeBaseID != aws.StringValue(input.KnowledgeBaseId) {
		return nil, errDataSourceNotFound(id)
	}
	return &bedrockagent.GetDataSourceOutput{DataSource: dataSourceRecordToOutput(*rec)}, nil
}

// ListDataSources returns knowledgeBaseId's data sources, sorted by creation
// time (id as a deterministic tie-breaker), mirroring ListKnowledgeBases.
func ListDataSources(ctx context.Context, accountID string, kb *handlers_ochrevector.KBStore, ds *handlers_ochrevector.DataSourceStore, input *bedrockagent.ListDataSourcesInput) (*bedrockagent.ListDataSourcesOutput, error) {
	if input == nil || aws.StringValue(input.KnowledgeBaseId) == "" {
		return nil, errors.New(awserrors.ErrorValidationException)
	}
	kbID := aws.StringValue(input.KnowledgeBaseId)
	kbRec, err := kb.Get(ctx, accountID, kbID)
	if err != nil {
		return nil, err
	}
	if kbRec == nil {
		return nil, errKBNotFound(kbID)
	}

	recs, err := ds.ListByKnowledgeBase(ctx, accountID, kbID)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(recs, func(a, b handlers_ochrevector.DataSourceRecord) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	summaries := make([]*bedrockagent.DataSourceSummary, 0, len(recs))
	for _, rec := range recs {
		summaries = append(summaries, &bedrockagent.DataSourceSummary{
			DataSourceId:    aws.String(rec.ID),
			Description:     nonEmptyStringPtr(rec.Description),
			KnowledgeBaseId: aws.String(rec.KnowledgeBaseID),
			Name:            aws.String(rec.Name),
			Status:          aws.String(dataSourceStatusToAWS(rec.Status)),
			UpdatedAt:       aws.Time(rec.UpdatedAt),
		})
	}
	return &bedrockagent.ListDataSourcesOutput{DataSourceSummaries: summaries}, nil
}

// DeleteDataSource removes dataSourceId scoped to knowledgeBaseId. Not
// idempotent on a foreign/unknown id (reports ResourceNotFoundException), the
// same contract as DeleteKnowledgeBase and real AWS's own DeleteDataSource.
func DeleteDataSource(ctx context.Context, accountID string, ds *handlers_ochrevector.DataSourceStore, input *bedrockagent.DeleteDataSourceInput) (*bedrockagent.DeleteDataSourceOutput, error) {
	if input == nil || aws.StringValue(input.KnowledgeBaseId) == "" || aws.StringValue(input.DataSourceId) == "" {
		return nil, errors.New(awserrors.ErrorValidationException)
	}
	id := aws.StringValue(input.DataSourceId)
	rec, err := ds.Get(ctx, accountID, id)
	if err != nil {
		return nil, err
	}
	if rec == nil || rec.KnowledgeBaseID != aws.StringValue(input.KnowledgeBaseId) {
		return nil, errDataSourceNotFound(id)
	}
	if err := ds.Delete(ctx, accountID, id); err != nil {
		return nil, err
	}
	return &bedrockagent.DeleteDataSourceOutput{
		DataSourceId:    aws.String(id),
		KnowledgeBaseId: input.KnowledgeBaseId,
		Status:          aws.String(bedrockagent.DataSourceStatusDeleting),
	}, nil
}

// jobRecordToOutput builds the AWS IngestionJob shape from job, scoped to the
// addressed kbID/dsID (JobRecord itself carries only IndexID, not an
// AWS-shaped knowledge-base/data-source id pair).
func jobRecordToOutput(kbID, dsID string, job handlers_ochrevector.JobRecord) *bedrockagent.IngestionJob {
	var failureReasons []*string
	for _, fd := range job.FailedDocuments {
		failureReasons = append(failureReasons, aws.String(fmt.Sprintf("%s: %s", fd.SourceKey, fd.Reason)))
	}
	if job.Error != "" {
		failureReasons = append(failureReasons, aws.String(job.Error))
	}
	return &bedrockagent.IngestionJob{
		DataSourceId:    aws.String(dsID),
		FailureReasons:  failureReasons,
		IngestionJobId:  aws.String(job.ID),
		KnowledgeBaseId: aws.String(kbID),
		StartedAt:       aws.Time(job.CreatedAt),
		Statistics: &bedrockagent.IngestionJobStatistics{
			NumberOfDocumentsFailed:          aws.Int64(int64(len(job.FailedDocuments))),
			NumberOfDocumentsScanned:         aws.Int64(int64(job.DocumentsTotal)),
			NumberOfNewDocumentsIndexed:      aws.Int64(int64(job.DocumentsDone)),
			NumberOfModifiedDocumentsIndexed: aws.Int64(0),
			NumberOfDocumentsDeleted:         aws.Int64(0),
		},
		Status:    aws.String(jobStateToAWS(job.State)),
		UpdatedAt: aws.Time(job.UpdatedAt),
	}
}

// StartIngestionJob builds an IngestRequest from dataSourceId's stored
// SourceSpec, targets knowledgeBaseId's bound index, and calls
// VectorService.Ingest.
func StartIngestionJob(ctx context.Context, accountID string, kb *handlers_ochrevector.KBStore, ds *handlers_ochrevector.DataSourceStore, vector handlers_ochrevector.VectorService, input *bedrockagent.StartIngestionJobInput) (*bedrockagent.StartIngestionJobOutput, error) {
	if input == nil || aws.StringValue(input.KnowledgeBaseId) == "" || aws.StringValue(input.DataSourceId) == "" {
		return nil, errors.New(awserrors.ErrorValidationException)
	}
	kbID := aws.StringValue(input.KnowledgeBaseId)
	dsID := aws.StringValue(input.DataSourceId)

	kbRec, err := kb.Get(ctx, accountID, kbID)
	if err != nil {
		return nil, err
	}
	if kbRec == nil {
		return nil, errKBNotFound(kbID)
	}

	dsRec, err := ds.Get(ctx, accountID, dsID)
	if err != nil {
		return nil, err
	}
	if dsRec == nil || dsRec.KnowledgeBaseID != kbID {
		return nil, errDataSourceNotFound(dsID)
	}

	resp, err := vector.Ingest(ctx, &handlers_ochrevector.IngestRequest{IndexID: kbRec.IndexID, Source: dsRec.Source}, accountID)
	if err != nil {
		return nil, translateVectorErr(err)
	}

	return &bedrockagent.StartIngestionJobOutput{IngestionJob: jobRecordToOutput(kbID, dsID, resp.Job)}, nil
}

// GetIngestionJob resolves jobId via VectorService.DescribeJob, then verifies
// it belongs to both the addressed knowledge base (its IndexID matches the
// KB's bound index) and the addressed data source (its Source matches the
// data source's own Source, see sourceSpecMatchesDataSource) before
// returning it, so a foreign/mismatched knowledgeBaseId or dataSourceId in
// the path cannot be used to read another KB/data source's job by guessing
// its id. The data-source check is the same Bucket+Prefix best-effort match
// ListIngestionJobs uses -- JobRecord carries no DataSourceID (see
// sourceSpecMatchesDataSource) -- so a real DataSourceID on JobRecord would
// close this exactly, but that crosses into the .9 ingest path and is left
// as a follow-up.
func GetIngestionJob(ctx context.Context, accountID string, kb *handlers_ochrevector.KBStore, ds *handlers_ochrevector.DataSourceStore, vector handlers_ochrevector.VectorService, input *bedrockagent.GetIngestionJobInput) (*bedrockagent.GetIngestionJobOutput, error) {
	if input == nil || aws.StringValue(input.KnowledgeBaseId) == "" || aws.StringValue(input.DataSourceId) == "" || aws.StringValue(input.IngestionJobId) == "" {
		return nil, errors.New(awserrors.ErrorValidationException)
	}
	kbID := aws.StringValue(input.KnowledgeBaseId)
	dsID := aws.StringValue(input.DataSourceId)
	jobID := aws.StringValue(input.IngestionJobId)

	kbRec, err := kb.Get(ctx, accountID, kbID)
	if err != nil {
		return nil, err
	}
	if kbRec == nil {
		return nil, errKBNotFound(kbID)
	}

	dsRec, err := ds.Get(ctx, accountID, dsID)
	if err != nil {
		return nil, err
	}
	if dsRec == nil || dsRec.KnowledgeBaseID != kbID {
		return nil, errDataSourceNotFound(dsID)
	}

	resp, err := vector.DescribeJob(ctx, &handlers_ochrevector.DescribeJobRequest{JobID: jobID}, accountID)
	if err != nil {
		return nil, translateVectorErr(err)
	}
	if resp.Job.IndexID != kbRec.IndexID || !sourceSpecMatchesDataSource(resp.Job.Source, dsRec.Source) {
		return nil, errIngestionJobNotFound(jobID)
	}

	return &bedrockagent.GetIngestionJobOutput{IngestionJob: jobRecordToOutput(kbID, dsID, resp.Job)}, nil
}

// sourceSpecMatchesDataSource reports whether an ingestion job's stored
// SourceSpec was built from ds's own SourceSpec (StartIngestionJob always
// builds the job's Source from the data source's Source verbatim, see
// StartIngestionJob above). Matched on Bucket+Prefix, the two fields that
// identify which documents a data source covers: JobRecord itself carries no
// DataSourceID (.9 has no such concept), so this is ListIngestionJobs' only
// way to scope a KB-wide job list down to one data source.
func sourceSpecMatchesDataSource(job, ds handlers_ochrevector.SourceSpec) bool {
	return job.Bucket == ds.Bucket && job.Prefix == ds.Prefix
}

// ListIngestionJobs lists knowledgeBaseId's jobs via the new
// VectorService.ListJobs, filtered to dataSourceId's own SourceSpec (see
// sourceSpecMatchesDataSource) and sorted by start time.
func ListIngestionJobs(ctx context.Context, accountID string, kb *handlers_ochrevector.KBStore, ds *handlers_ochrevector.DataSourceStore, vector handlers_ochrevector.VectorService, input *bedrockagent.ListIngestionJobsInput) (*bedrockagent.ListIngestionJobsOutput, error) {
	if input == nil || aws.StringValue(input.KnowledgeBaseId) == "" || aws.StringValue(input.DataSourceId) == "" {
		return nil, errors.New(awserrors.ErrorValidationException)
	}
	kbID := aws.StringValue(input.KnowledgeBaseId)
	dsID := aws.StringValue(input.DataSourceId)

	kbRec, err := kb.Get(ctx, accountID, kbID)
	if err != nil {
		return nil, err
	}
	if kbRec == nil {
		return nil, errKBNotFound(kbID)
	}

	dsRec, err := ds.Get(ctx, accountID, dsID)
	if err != nil {
		return nil, err
	}
	if dsRec == nil || dsRec.KnowledgeBaseID != kbID {
		return nil, errDataSourceNotFound(dsID)
	}

	resp, err := vector.ListJobs(ctx, &handlers_ochrevector.ListJobsRequest{}, accountID)
	if err != nil {
		return nil, translateVectorErr(err)
	}

	summaries := make([]*bedrockagent.IngestionJobSummary, 0)
	for _, job := range resp.Jobs {
		if job.IndexID != kbRec.IndexID || !sourceSpecMatchesDataSource(job.Source, dsRec.Source) {
			continue
		}
		out := jobRecordToOutput(kbID, dsID, job)
		summaries = append(summaries, &bedrockagent.IngestionJobSummary{
			DataSourceId:    out.DataSourceId,
			IngestionJobId:  out.IngestionJobId,
			KnowledgeBaseId: out.KnowledgeBaseId,
			StartedAt:       out.StartedAt,
			Statistics:      out.Statistics,
			Status:          out.Status,
			UpdatedAt:       out.UpdatedAt,
		})
	}
	slices.SortFunc(summaries, func(a, b *bedrockagent.IngestionJobSummary) int {
		return a.StartedAt.Compare(*b.StartedAt)
	})
	return &bedrockagent.ListIngestionJobsOutput{IngestionJobSummaries: summaries}, nil
}
