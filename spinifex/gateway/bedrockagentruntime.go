package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrockagentruntime"
	"github.com/aws/aws-sdk-go/service/bedrockruntime"
	"github.com/google/uuid"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_bedrock "github.com/mulgadc/spinifex/spinifex/gateway/bedrock"
	handlers_ochrevector "github.com/mulgadc/spinifex/spinifex/handlers/ochrevector"
)

// converseFn is gateway_bedrock.Converse partially applied over the request's
// resolved dependencies (credential/endpoint/access/provisioned/guardrail
// resolvers), so a route handler needs only this one parameter to reach the
// RetrieveAndGenerate generation step, mirroring how BedrockRuntime_Request
// resolves the same six dependencies once per request.
type converseFn func(ctx context.Context, accountID, modelID string, input *bedrockruntime.ConverseInput) (*bedrockruntime.ConverseOutput, error)

// bedrockAgentRuntimeRoute maps one HTTP method + path regex to an AWS action
// and handler, mirroring bedrockAgentRoute.
type bedrockAgentRuntimeRoute struct {
	method  string
	pattern *regexp.Regexp
	action  string
	handler bedrockAgentRuntimeRouteHandler
}

// bedrockAgentRuntimeRouteHandler invokes a per-action bedrock-agent-runtime
// (data-plane) gateway function. params holds the regex capture groups,
// PathUnescape'd. body is the raw request bytes, needed alongside the typed
// SDK input because RetrievalFilter's leaf operators must be recovered from
// it directly (see wireFilter). kb/vector are gw.BedrockAgentKB/
// gw.BedrockAgentVector, the same stores bedrock-agent's control plane uses.
// converse reaches gateway_bedrock.Converse for RetrieveAndGenerate's
// generation step.
type bedrockAgentRuntimeRouteHandler func(ctx context.Context, accountID string, params []string, body []byte, kb *handlers_ochrevector.KBStore, vector handlers_ochrevector.VectorService, converse converseFn) (any, error)

// bedrockAgentRuntimeRoutes is the dispatch table. Real AWS HTTP paths/methods,
// verified against the vendored aws-sdk-go bedrockagentruntime request
// definitions (opRetrieve/opRetrieveAndGenerate).
var bedrockAgentRuntimeRoutes = []bedrockAgentRuntimeRoute{
	{"POST", regexp.MustCompile(`^/knowledgebases/([^/]+)/retrieve$`), "Retrieve",
		func(ctx context.Context, acct string, p []string, b []byte, kb *handlers_ochrevector.KBStore, vector handlers_ochrevector.VectorService, _ converseFn) (any, error) {
			input := new(bedrockagentruntime.RetrieveInput)
			if len(b) > 0 {
				if err := json.Unmarshal(b, input); err != nil {
					return nil, errors.New(awserrors.ErrorValidationException)
				}
			}
			input.KnowledgeBaseId = aws.String(p[0])
			return Retrieve(ctx, acct, kb, vector, b, input)
		}},
	{"POST", regexp.MustCompile(`^/retrieveAndGenerate$`), "RetrieveAndGenerate",
		func(ctx context.Context, acct string, _ []string, b []byte, kb *handlers_ochrevector.KBStore, vector handlers_ochrevector.VectorService, converse converseFn) (any, error) {
			input := new(bedrockagentruntime.RetrieveAndGenerateInput)
			if len(b) > 0 {
				if err := json.Unmarshal(b, input); err != nil {
					return nil, errors.New(awserrors.ErrorValidationException)
				}
			}
			return RetrieveAndGenerate(ctx, acct, kb, vector, converse, b, input)
		}},
}

// lookupBedrockAgentRuntimeAction matches method+path against
// bedrockAgentRuntimeRoutes, mirroring lookupBedrockAgentAction.
func lookupBedrockAgentRuntimeAction(method, path string) (string, []string, bedrockAgentRuntimeRouteHandler, bool) {
	for _, route := range bedrockAgentRuntimeRoutes {
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
					slog.Debug("bedrock-agent-runtime: bad percent-encoding in path param", "param", raw, "err", err)
					decoded = raw
				}
				params = append(params, decoded)
			}
		}
		return route.action, params, route.handler, true
	}
	return "", nil, nil, false
}

// BedrockAgentRuntime_Request dispatches bedrock-agent-runtime (data-plane)
// REST-JSON requests: resolves method+path to an action, reads the body,
// calls the handler, and serialises the output as JSON, mirroring
// BedrockAgent_Request. RetrieveAndGenerate is metered the same way Converse
// is on bedrock-runtime (it ends up calling gateway_bedrock.Converse
// in-process); Retrieve never reaches a model, so it is not metered.
func (gw *GatewayConfig) BedrockAgentRuntime_Request(w http.ResponseWriter, r *http.Request) error {
	action, params, handler, ok := lookupBedrockAgentRuntimeAction(r.Method, r.URL.EscapedPath())
	if !ok {
		slog.DebugContext(r.Context(), "bedrock-agent-runtime: no route for request", "method", r.Method, "path", r.URL.Path)
		return errors.New(awserrors.ErrorInvalidAction)
	}

	if err := gw.checkPolicy(r, "bedrock-agent-runtime", action); err != nil {
		return err
	}

	if gw.BedrockAgentKB == nil || gw.BedrockAgentVector == nil {
		return errors.New(awserrors.ErrorServerInternal)
	}

	accountID, _ := r.Context().Value(ctxAccountID).(string)
	if accountID == "" {
		slog.ErrorContext(r.Context(), "BedrockAgentRuntime_Request: no account ID in auth context")
		return errors.New(awserrors.ErrorServerInternal)
	}

	if action == "RetrieveAndGenerate" {
		if err := gw.Quota.CheckBedrockRPM(accountID); err != nil {
			return err
		}
		if err := gw.Quota.CheckBedrockTokens(r.Context(), accountID); err != nil {
			return err
		}
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.ErrorContext(r.Context(), "BedrockAgentRuntime_Request: failed to read body", "err", err)
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	converse := func(ctx context.Context, acct, modelID string, input *bedrockruntime.ConverseInput) (*bedrockruntime.ConverseOutput, error) {
		return gateway_bedrock.Converse(ctx, acct, modelID, input, gw.bedrockResolver(), gw.bedrockEndpointResolver(), gw.bedrockRecorder(), gw.bedrockAccessResolver(), gw.bedrockProvisionedStore(), gw.bedrockGuardrailStore())
	}

	output, err := handler(r.Context(), accountID, params, body, gw.BedrockAgentKB, gw.BedrockAgentVector, converse)
	if err != nil {
		return err
	}

	gateway_bedrock.WriteJSONResponse(w, output)
	return nil
}

// wireFilterAttr is one leaf filter operator's {key, value} pair off the raw
// request body. Value is untyped because its shape depends on the operator:
// a scalar for equals/notEquals/greaterThan(OrEquals)/lessThan(OrEquals)/
// startsWith/stringContains/listContains, a list for in/notIn.
type wireFilterAttr struct {
	Key   string `json:"key"`
	Value any    `json:"value"`
}

// wireFilter mirrors the full real-AWS bedrock-agent-runtime RetrievalFilter
// wire shape's operator set (D9: equals/notEquals/greaterThan(OrEquals)/
// lessThan(OrEquals)/in/notIn/startsWith/stringContains/listContains/andAll/
// orAll).
//
// It exists because the vendored github.com/aws/aws-sdk-go v1.55.8
// bedrockagentruntime.RetrievalFilter Go struct carries ONLY AndAll/OrAll —
// its leaf-operator fields were never added to this SDK major version (aws-
// sdk-go v1 is in maintenance mode and this filter shape landed after that).
// json.Unmarshal into the vendored type silently drops every leaf condition
// rather than erroring, which would make a caller's metadata filter silently
// no-op instead of scoping results -- a correctness/security-relevant gap
// (e.g. tenant- or document-class-scoping filters would silently stop
// filtering), not just a missing echo field. This shadow type recovers the
// full operator set directly from the raw request body instead.
type wireFilter struct {
	Equals              *wireFilterAttr `json:"equals,omitempty"`
	NotEquals           *wireFilterAttr `json:"notEquals,omitempty"`
	GreaterThan         *wireFilterAttr `json:"greaterThan,omitempty"`
	GreaterThanOrEquals *wireFilterAttr `json:"greaterThanOrEquals,omitempty"`
	LessThan            *wireFilterAttr `json:"lessThan,omitempty"`
	LessThanOrEquals    *wireFilterAttr `json:"lessThanOrEquals,omitempty"`
	In                  *wireFilterAttr `json:"in,omitempty"`
	NotIn               *wireFilterAttr `json:"notIn,omitempty"`
	StartsWith          *wireFilterAttr `json:"startsWith,omitempty"`
	StringContains      *wireFilterAttr `json:"stringContains,omitempty"`
	ListContains        *wireFilterAttr `json:"listContains,omitempty"`
	AndAll              []wireFilter    `json:"andAll,omitempty"`
	OrAll               []wireFilter    `json:"orAll,omitempty"`
}

// isZero reports whether f carries no operator at all, the shape an absent
// filter or an empty JSON object ("filter":{}) decodes to.
func (f *wireFilter) isZero() bool {
	return f == nil || (f.Equals == nil && f.NotEquals == nil && f.GreaterThan == nil &&
		f.GreaterThanOrEquals == nil && f.LessThan == nil && f.LessThanOrEquals == nil &&
		f.In == nil && f.NotIn == nil && f.StartsWith == nil && f.StringContains == nil &&
		f.ListContains == nil && len(f.AndAll) == 0 && len(f.OrAll) == 0)
}

// toFilter translates f onto ochrevector's Filter AST 1:1 by operator name
// (D9), recursing into andAll/orAll. AWS's RetrievalFilter is a oneof (exactly
// one operator per node); an over-specified node resolves the first non-nil
// field in the fixed order below rather than erroring, consistent with how
// the rest of this gateway treats an over-specified request as best-effort.
func (f *wireFilter) toFilter() (*handlers_ochrevector.Filter, error) {
	if f.isZero() {
		return nil, nil
	}
	switch {
	case f.Equals != nil:
		return handlers_ochrevector.Equals(f.Equals.Key, f.Equals.Value), nil
	case f.NotEquals != nil:
		return handlers_ochrevector.NotEquals(f.NotEquals.Key, f.NotEquals.Value), nil
	case f.GreaterThan != nil:
		return handlers_ochrevector.GreaterThan(f.GreaterThan.Key, f.GreaterThan.Value), nil
	case f.GreaterThanOrEquals != nil:
		return handlers_ochrevector.GreaterThanOrEquals(f.GreaterThanOrEquals.Key, f.GreaterThanOrEquals.Value), nil
	case f.LessThan != nil:
		return handlers_ochrevector.LessThan(f.LessThan.Key, f.LessThan.Value), nil
	case f.LessThanOrEquals != nil:
		return handlers_ochrevector.LessThanOrEquals(f.LessThanOrEquals.Key, f.LessThanOrEquals.Value), nil
	case f.In != nil:
		values, err := wireFilterStringSlice(f.In.Value)
		if err != nil {
			return nil, fmt.Errorf("bedrock-agent-runtime: filter \"in\" on %q: %w", f.In.Key, err)
		}
		return handlers_ochrevector.In(f.In.Key, values), nil
	case f.NotIn != nil:
		values, err := wireFilterStringSlice(f.NotIn.Value)
		if err != nil {
			return nil, fmt.Errorf("bedrock-agent-runtime: filter \"notIn\" on %q: %w", f.NotIn.Key, err)
		}
		return handlers_ochrevector.NotIn(f.NotIn.Key, values), nil
	case f.StartsWith != nil:
		prefix, ok := f.StartsWith.Value.(string)
		if !ok {
			return nil, fmt.Errorf("bedrock-agent-runtime: filter \"startsWith\" on %q requires a string value", f.StartsWith.Key)
		}
		return handlers_ochrevector.StartsWith(f.StartsWith.Key, prefix), nil
	case f.StringContains != nil:
		substr, ok := f.StringContains.Value.(string)
		if !ok {
			return nil, fmt.Errorf("bedrock-agent-runtime: filter \"stringContains\" on %q requires a string value", f.StringContains.Key)
		}
		return handlers_ochrevector.StringContains(f.StringContains.Key, substr), nil
	case f.ListContains != nil:
		value, ok := f.ListContains.Value.(string)
		if !ok {
			return nil, fmt.Errorf("bedrock-agent-runtime: filter \"listContains\" on %q requires a string value", f.ListContains.Key)
		}
		return handlers_ochrevector.ListContains(f.ListContains.Key, value), nil
	case len(f.AndAll) > 0:
		children, err := wireFiltersToChildren(f.AndAll)
		if err != nil {
			return nil, err
		}
		return handlers_ochrevector.AndAll(children...), nil
	default: // len(f.OrAll) > 0, the only remaining non-zero case.
		children, err := wireFiltersToChildren(f.OrAll)
		if err != nil {
			return nil, err
		}
		return handlers_ochrevector.OrAll(children...), nil
	}
}

// wireFiltersToChildren translates a combinator's child list, rejecting any
// child that itself carries no operator (a malformed request, not a filter
// that matches nothing).
func wireFiltersToChildren(fs []wireFilter) ([]*handlers_ochrevector.Filter, error) {
	children := make([]*handlers_ochrevector.Filter, 0, len(fs))
	for i := range fs {
		child, err := fs[i].toFilter()
		if err != nil {
			return nil, err
		}
		if child == nil {
			return nil, errors.New("bedrock-agent-runtime: empty filter condition in combinator")
		}
		children = append(children, child)
	}
	return children, nil
}

// wireFilterStringSlice coerces a decoded JSON value (a []any of strings
// after json.Unmarshal, since encoding/json has no []string-specific
// decoding for an any field) to []string for in/notIn.
func wireFilterStringSlice(v any) ([]string, error) {
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("requires a list value, got %T", v)
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("requires a list of strings, got %T element", item)
		}
		out = append(out, s)
	}
	return out, nil
}

// retrieveFilterEnvelope decodes only the nested filter object from a
// Retrieve request body, bypassing the vendored SDK's leaf-incomplete
// RetrievalFilter type (see wireFilter's doc comment for why).
type retrieveFilterEnvelope struct {
	RetrievalConfiguration *struct {
		VectorSearchConfiguration *struct {
			Filter *wireFilter `json:"filter"`
		} `json:"vectorSearchConfiguration"`
	} `json:"retrievalConfiguration"`
}

// decodeRetrieveFilter extracts and translates Retrieve's filter from the raw
// request body, returning (nil, nil) when no filter was sent.
func decodeRetrieveFilter(body []byte) (*handlers_ochrevector.Filter, error) {
	if len(body) == 0 {
		return nil, nil
	}
	var env retrieveFilterEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, err
	}
	if env.RetrievalConfiguration == nil || env.RetrievalConfiguration.VectorSearchConfiguration == nil {
		return nil, nil
	}
	return env.RetrievalConfiguration.VectorSearchConfiguration.Filter.toFilter()
}

// retrieveAndGenerateFilterEnvelope is decodeRetrieveFilter's sibling for
// RetrieveAndGenerate's more deeply nested
// retrieveAndGenerateConfiguration.knowledgeBaseConfiguration.
// retrievalConfiguration.vectorSearchConfiguration.filter path.
type retrieveAndGenerateFilterEnvelope struct {
	RetrieveAndGenerateConfiguration *struct {
		KnowledgeBaseConfiguration *struct {
			RetrievalConfiguration *struct {
				VectorSearchConfiguration *struct {
					Filter *wireFilter `json:"filter"`
				} `json:"vectorSearchConfiguration"`
			} `json:"retrievalConfiguration"`
		} `json:"knowledgeBaseConfiguration"`
	} `json:"retrieveAndGenerateConfiguration"`
}

// decodeRetrieveAndGenerateFilter is decodeRetrieveFilter's sibling for
// RetrieveAndGenerate.
func decodeRetrieveAndGenerateFilter(body []byte) (*handlers_ochrevector.Filter, error) {
	if len(body) == 0 {
		return nil, nil
	}
	var env retrieveAndGenerateFilterEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, err
	}
	cfg := env.RetrieveAndGenerateConfiguration
	if cfg == nil || cfg.KnowledgeBaseConfiguration == nil || cfg.KnowledgeBaseConfiguration.RetrievalConfiguration == nil ||
		cfg.KnowledgeBaseConfiguration.RetrievalConfiguration.VectorSearchConfiguration == nil {
		return nil, nil
	}
	return cfg.KnowledgeBaseConfiguration.RetrievalConfiguration.VectorSearchConfiguration.Filter.toFilter()
}

// queryResultToRetrievalResult maps one ochrevector QueryResult onto AWS's
// KnowledgeBaseRetrievalResult shape. Uri is set to SourceKey verbatim (the
// S3 object key .9 recorded at ingest, not a bucket-qualified s3:// URI --
// .9's QueryResult carries no bucket). Metadata is omitted: the vendored SDK's
// KnowledgeBaseRetrievalResult, unlike real AWS's, carries no metadata field
// to put it in (same SDK-vintage gap as wireFilter, but here it only drops an
// echo of already-visible data rather than silently changing behaviour, so it
// is left unset rather than worked around).
func queryResultToRetrievalResult(r handlers_ochrevector.QueryResult) *bedrockagentruntime.KnowledgeBaseRetrievalResult {
	return &bedrockagentruntime.KnowledgeBaseRetrievalResult{
		Content: &bedrockagentruntime.RetrievalResultContent{Text: aws.String(r.Chunk)},
		Location: &bedrockagentruntime.RetrievalResultLocation{
			Type:       aws.String(bedrockagentruntime.RetrievalResultLocationTypeS3),
			S3Location: &bedrockagentruntime.RetrievalResultS3Location{Uri: aws.String(r.SourceKey)},
		},
		Score: aws.Float64(float64(r.Score)),
	}
}

// Retrieve resolves knowledgeBaseId's bound index and runs a similarity query
// against it: retrievalQuery.text -> Text, numberOfResults -> K, filter ->
// translate() onto .9's Filter AST. Phase 5 is single-page (D2's scope is
// ListIngestionJobs, not Retrieve pagination): NextToken is never set on the
// response, and an incoming NextToken is accepted and ignored rather than
// rejected, matching how storageConfiguration/roleArn are accepted-and-
// stubbed elsewhere in this gateway.
func Retrieve(ctx context.Context, accountID string, kb *handlers_ochrevector.KBStore, vector handlers_ochrevector.VectorService, body []byte, input *bedrockagentruntime.RetrieveInput) (*bedrockagentruntime.RetrieveOutput, error) {
	if input == nil || aws.StringValue(input.KnowledgeBaseId) == "" || input.RetrievalQuery == nil || aws.StringValue(input.RetrievalQuery.Text) == "" {
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

	numResults := 0
	if input.RetrievalConfiguration != nil && input.RetrievalConfiguration.VectorSearchConfiguration != nil {
		numResults = int(aws.Int64Value(input.RetrievalConfiguration.VectorSearchConfiguration.NumberOfResults))
	}

	filter, err := decodeRetrieveFilter(body)
	if err != nil {
		return nil, errors.New(awserrors.ErrorValidationException)
	}

	resp, err := vector.Query(ctx, &handlers_ochrevector.QueryRequest{
		IndexID: kbRec.IndexID,
		Text:    aws.StringValue(input.RetrievalQuery.Text),
		K:       numResults,
		Filter:  filter,
	}, accountID)
	if err != nil {
		return nil, translateVectorErr(err)
	}

	results := make([]*bedrockagentruntime.KnowledgeBaseRetrievalResult, 0, len(resp.Results))
	for _, r := range resp.Results {
		results = append(results, queryResultToRetrievalResult(r))
	}
	return &bedrockagentruntime.RetrieveOutput{RetrievalResults: results}, nil
}

// defaultRAGPromptTemplate is Phase 5's default prompt when the caller omits
// generationConfiguration.promptTemplate, following AWS's own
// $search_results$/$query$ placeholder convention (renderRAGPrompt) so a
// caller-supplied template is a drop-in replacement.
const defaultRAGPromptTemplate = "You are a question answering agent. I will provide you with a set of search results. " +
	"The user will provide you with a question. Your job is to answer the user's question using only information " +
	"from the search results. If the search results do not contain information that can answer the question, " +
	"say that you could not find an exact answer to the question.\n\nHere are the search results:\n$search_results$\n\nQuestion: $query$"

// ragSearchResultsPlaceholder and ragQueryPlaceholder are AWS's standard
// RetrieveAndGenerate prompt-template placeholders (see the Bedrock user
// guide's default knowledge-base prompt template).
const (
	ragSearchResultsPlaceholder = "$search_results$"
	ragQueryPlaceholder         = "$query$"
)

// renderRAGPrompt substitutes AWS's standard placeholders into template,
// concatenating chunks with a blank line between each so the model can tell
// separate sources apart.
func renderRAGPrompt(template, query string, chunks []string) string {
	out := strings.ReplaceAll(template, ragSearchResultsPlaceholder, strings.Join(chunks, "\n\n"))
	return strings.ReplaceAll(out, ragQueryPlaceholder, query)
}

// converseOutputText concatenates every text content block of out's message,
// the shape a Converse response carrying only text (no tool use/images)
// always reduces to.
func converseOutputText(out *bedrockruntime.ConverseOutput) string {
	if out == nil || out.Output == nil || out.Output.Message == nil {
		return ""
	}
	parts := make([]string, 0, len(out.Output.Message.Content))
	for _, c := range out.Output.Message.Content {
		if c != nil && c.Text != nil {
			parts = append(parts, aws.StringValue(c.Text))
		}
	}
	return strings.Join(parts, "")
}

// RetrieveAndGenerate runs Retrieve's own flow for top-k chunks, assembles a
// Converse prompt (D-RAG: a system message carrying the rendered prompt
// template + retrieved context, the raw query text as the user turn), calls
// converse in-process for generation, and builds one aggregate citation from
// the retrieved chunks. D6: sessionId is server-generated when the caller
// omits it, and echoed either way -- no conversational memory is persisted,
// so a reused sessionId has no effect on this or any later call.
func RetrieveAndGenerate(ctx context.Context, accountID string, kb *handlers_ochrevector.KBStore, vector handlers_ochrevector.VectorService, converse converseFn, body []byte, input *bedrockagentruntime.RetrieveAndGenerateInput) (*bedrockagentruntime.RetrieveAndGenerateOutput, error) {
	if input == nil || input.Input == nil || aws.StringValue(input.Input.Text) == "" {
		return nil, errors.New(awserrors.ErrorValidationException)
	}
	ragConfig := input.RetrieveAndGenerateConfiguration
	if ragConfig == nil || aws.StringValue(ragConfig.Type) != bedrockagentruntime.RetrieveAndGenerateTypeKnowledgeBase || ragConfig.KnowledgeBaseConfiguration == nil {
		return nil, awserrors.Errorf(awserrors.ErrorValidationException, "bedrock-agent-runtime: only KNOWLEDGE_BASE retrieveAndGenerate is supported")
	}
	kbConfig := ragConfig.KnowledgeBaseConfiguration
	kbID := aws.StringValue(kbConfig.KnowledgeBaseId)
	modelArn := aws.StringValue(kbConfig.ModelArn)
	if kbID == "" || modelArn == "" {
		return nil, errors.New(awserrors.ErrorValidationException)
	}

	kbRec, err := kb.Get(ctx, accountID, kbID)
	if err != nil {
		return nil, err
	}
	if kbRec == nil {
		return nil, errKBNotFound(kbID)
	}

	numResults := 0
	if kbConfig.RetrievalConfiguration != nil && kbConfig.RetrievalConfiguration.VectorSearchConfiguration != nil {
		numResults = int(aws.Int64Value(kbConfig.RetrievalConfiguration.VectorSearchConfiguration.NumberOfResults))
	}

	filter, err := decodeRetrieveAndGenerateFilter(body)
	if err != nil {
		return nil, errors.New(awserrors.ErrorValidationException)
	}

	queryText := aws.StringValue(input.Input.Text)
	queryResp, err := vector.Query(ctx, &handlers_ochrevector.QueryRequest{
		IndexID: kbRec.IndexID,
		Text:    queryText,
		K:       numResults,
		Filter:  filter,
	}, accountID)
	if err != nil {
		return nil, translateVectorErr(err)
	}

	results := make([]*bedrockagentruntime.KnowledgeBaseRetrievalResult, 0, len(queryResp.Results))
	chunks := make([]string, 0, len(queryResp.Results))
	for _, r := range queryResp.Results {
		results = append(results, queryResultToRetrievalResult(r))
		chunks = append(chunks, r.Chunk)
	}

	template := defaultRAGPromptTemplate
	if kbConfig.GenerationConfiguration != nil && kbConfig.GenerationConfiguration.PromptTemplate != nil {
		if tp := aws.StringValue(kbConfig.GenerationConfiguration.PromptTemplate.TextPromptTemplate); tp != "" {
			template = tp
		}
	}
	systemText := renderRAGPrompt(template, queryText, chunks)

	// embeddingModelIDFromARN is generic ARN-suffix extraction (not actually
	// embedding-specific despite its name), reused here for modelArn rather
	// than duplicating the same "foundation-model/" parsing.
	modelID := embeddingModelIDFromARN(modelArn)
	converseInput := &bedrockruntime.ConverseInput{
		System: []*bedrockruntime.SystemContentBlock{{Text: aws.String(systemText)}},
		Messages: []*bedrockruntime.Message{{
			Role:    aws.String(bedrockruntime.ConversationRoleUser),
			Content: []*bedrockruntime.ContentBlock{{Text: aws.String(queryText)}},
		}},
	}
	if kbConfig.GenerationConfiguration != nil && kbConfig.GenerationConfiguration.InferenceConfig != nil && kbConfig.GenerationConfiguration.InferenceConfig.TextInferenceConfig != nil {
		tic := kbConfig.GenerationConfiguration.InferenceConfig.TextInferenceConfig
		converseInput.InferenceConfig = &bedrockruntime.InferenceConfiguration{
			MaxTokens:     tic.MaxTokens,
			StopSequences: tic.StopSequences,
			Temperature:   tic.Temperature,
			TopP:          tic.TopP,
		}
	}

	out, err := converse(ctx, accountID, modelID, converseInput)
	if err != nil {
		return nil, err
	}
	outputText := converseOutputText(out)

	sessionID := aws.StringValue(input.SessionId)
	if sessionID == "" {
		sessionID = uuid.NewString()
	}

	// One aggregate citation covering the whole generated answer: the
	// underlying Converse call is a plain chat completion with no span-level
	// attribution signal, so there is no way to know which retrieved chunk
	// backs which part of the model's answer.
	var citations []*bedrockagentruntime.Citation
	if len(results) > 0 {
		refs := make([]*bedrockagentruntime.RetrievedReference, 0, len(results))
		for _, r := range results {
			refs = append(refs, &bedrockagentruntime.RetrievedReference{Content: r.Content, Location: r.Location})
		}
		citations = []*bedrockagentruntime.Citation{{
			GeneratedResponsePart: &bedrockagentruntime.GeneratedResponsePart{
				TextResponsePart: &bedrockagentruntime.TextResponsePart{Text: aws.String(outputText)},
			},
			RetrievedReferences: refs,
		}}
	}

	return &bedrockagentruntime.RetrieveAndGenerateOutput{
		Citations: citations,
		Output:    &bedrockagentruntime.RetrieveAndGenerateOutput_{Text: aws.String(outputText)},
		SessionId: aws.String(sessionID),
	}, nil
}
