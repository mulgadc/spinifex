// In-package: exercises bedrockagentruntime.go's unexported filter
// translator (wireFilter) and the exported Retrieve/RetrieveAndGenerate
// mapping functions directly, mirroring bedrockagent_test.go's split between
// unexported-mapper and operation-level coverage.
//
//test:in-package
package gateway

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrockagentruntime"
	"github.com/aws/aws-sdk-go/service/bedrockruntime"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ochrevector "github.com/mulgadc/spinifex/spinifex/handlers/ochrevector"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWireFilter_ToFilter_Leaves(t *testing.T) {
	cases := []struct {
		name   string
		filter wireFilter
		wantOp handlers_ochrevector.FilterOp
		wantK  string
		wantV  any
	}{
		{"equals", wireFilter{Equals: &wireFilterAttr{Key: "genre", Value: "fiction"}}, handlers_ochrevector.FilterEquals, "genre", "fiction"},
		{"notEquals", wireFilter{NotEquals: &wireFilterAttr{Key: "genre", Value: "fiction"}}, handlers_ochrevector.FilterNotEquals, "genre", "fiction"},
		{"greaterThan", wireFilter{GreaterThan: &wireFilterAttr{Key: "year", Value: 2000.0}}, handlers_ochrevector.FilterGreaterThan, "year", 2000.0},
		{"greaterThanOrEquals", wireFilter{GreaterThanOrEquals: &wireFilterAttr{Key: "year", Value: 2000.0}}, handlers_ochrevector.FilterGreaterThanOrEqual, "year", 2000.0},
		{"lessThan", wireFilter{LessThan: &wireFilterAttr{Key: "year", Value: 2000.0}}, handlers_ochrevector.FilterLessThan, "year", 2000.0},
		{"lessThanOrEquals", wireFilter{LessThanOrEquals: &wireFilterAttr{Key: "year", Value: 2000.0}}, handlers_ochrevector.FilterLessThanOrEqual, "year", 2000.0},
		{"startsWith", wireFilter{StartsWith: &wireFilterAttr{Key: "title", Value: "The "}}, handlers_ochrevector.FilterStartsWith, "title", "The "},
		{"stringContains", wireFilter{StringContains: &wireFilterAttr{Key: "title", Value: "dragon"}}, handlers_ochrevector.FilterStringContains, "title", "dragon"},
		{"listContains", wireFilter{ListContains: &wireFilterAttr{Key: "tags", Value: "scifi"}}, handlers_ochrevector.FilterListContains, "tags", "scifi"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.filter.toFilter()
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, tc.wantOp, got.Op)
			assert.Equal(t, tc.wantK, got.Key)
			assert.Equal(t, tc.wantV, got.Value)
		})
	}
}

func TestWireFilter_ToFilter_InNotIn(t *testing.T) {
	f := wireFilter{In: &wireFilterAttr{Key: "genre", Value: []any{"fiction", "scifi"}}}
	got, err := f.toFilter()
	require.NoError(t, err)
	assert.Equal(t, handlers_ochrevector.FilterIn, got.Op)
	assert.Equal(t, []string{"fiction", "scifi"}, got.Value)

	f = wireFilter{NotIn: &wireFilterAttr{Key: "genre", Value: []any{"horror"}}}
	got, err = f.toFilter()
	require.NoError(t, err)
	assert.Equal(t, handlers_ochrevector.FilterNotIn, got.Op)
	assert.Equal(t, []string{"horror"}, got.Value)
}

func TestWireFilter_ToFilter_InRequiresList(t *testing.T) {
	f := wireFilter{In: &wireFilterAttr{Key: "genre", Value: "fiction"}}
	_, err := f.toFilter()
	assert.Error(t, err)
}

func TestWireFilter_ToFilter_AndOrAll(t *testing.T) {
	f := wireFilter{AndAll: []wireFilter{
		{Equals: &wireFilterAttr{Key: "genre", Value: "fiction"}},
		{GreaterThan: &wireFilterAttr{Key: "year", Value: 2000.0}},
	}}
	got, err := f.toFilter()
	require.NoError(t, err)
	assert.Equal(t, handlers_ochrevector.FilterAndAll, got.Op)
	require.Len(t, got.Children, 2)
	assert.Equal(t, handlers_ochrevector.FilterEquals, got.Children[0].Op)
	assert.Equal(t, handlers_ochrevector.FilterGreaterThan, got.Children[1].Op)

	f = wireFilter{OrAll: []wireFilter{
		{Equals: &wireFilterAttr{Key: "genre", Value: "fiction"}},
		{Equals: &wireFilterAttr{Key: "genre", Value: "scifi"}},
	}}
	got, err = f.toFilter()
	require.NoError(t, err)
	assert.Equal(t, handlers_ochrevector.FilterOrAll, got.Op)
	require.Len(t, got.Children, 2)
}

func TestWireFilter_ToFilter_NestedCombinator(t *testing.T) {
	f := wireFilter{AndAll: []wireFilter{
		{Equals: &wireFilterAttr{Key: "genre", Value: "fiction"}},
		{OrAll: []wireFilter{
			{Equals: &wireFilterAttr{Key: "lang", Value: "en"}},
			{Equals: &wireFilterAttr{Key: "lang", Value: "fr"}},
		}},
	}}
	got, err := f.toFilter()
	require.NoError(t, err)
	require.Len(t, got.Children, 2)
	assert.Equal(t, handlers_ochrevector.FilterOrAll, got.Children[1].Op)
	require.Len(t, got.Children[1].Children, 2)
}

func TestWireFilter_ToFilter_EmptyChildRejected(t *testing.T) {
	f := wireFilter{AndAll: []wireFilter{{}, {Equals: &wireFilterAttr{Key: "x", Value: "y"}}}}
	_, err := f.toFilter()
	assert.Error(t, err)
}

func TestWireFilter_ToFilter_NilOrEmptyIsNoFilter(t *testing.T) {
	var f *wireFilter
	got, err := f.toFilter()
	require.NoError(t, err)
	assert.Nil(t, got)

	f = &wireFilter{}
	got, err = f.toFilter()
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestDecodeRetrieveFilter(t *testing.T) {
	body := []byte(`{
		"knowledgeBaseId": "kb-1",
		"retrievalQuery": {"text": "dragons"},
		"retrievalConfiguration": {
			"vectorSearchConfiguration": {
				"numberOfResults": 5,
				"filter": {"equals": {"key": "genre", "value": "fiction"}}
			}
		}
	}`)
	f, err := decodeRetrieveFilter(body)
	require.NoError(t, err)
	require.NotNil(t, f)
	assert.Equal(t, handlers_ochrevector.FilterEquals, f.Op)
	assert.Equal(t, "genre", f.Key)
}

func TestDecodeRetrieveFilter_Absent(t *testing.T) {
	f, err := decodeRetrieveFilter([]byte(`{"knowledgeBaseId": "kb-1", "retrievalQuery": {"text": "dragons"}}`))
	require.NoError(t, err)
	assert.Nil(t, f)

	f, err = decodeRetrieveFilter(nil)
	require.NoError(t, err)
	assert.Nil(t, f)
}

func TestDecodeRetrieveAndGenerateFilter(t *testing.T) {
	body := []byte(`{
		"input": {"text": "who is the dragon?"},
		"retrieveAndGenerateConfiguration": {
			"type": "KNOWLEDGE_BASE",
			"knowledgeBaseConfiguration": {
				"knowledgeBaseId": "kb-1",
				"modelArn": "anthropic.claude-3-5-sonnet",
				"retrievalConfiguration": {
					"vectorSearchConfiguration": {
						"filter": {"andAll": [
							{"equals": {"key": "genre", "value": "fiction"}},
							{"in": {"key": "lang", "value": ["en", "fr"]}}
						]}
					}
				}
			}
		}
	}`)
	f, err := decodeRetrieveAndGenerateFilter(body)
	require.NoError(t, err)
	require.NotNil(t, f)
	assert.Equal(t, handlers_ochrevector.FilterAndAll, f.Op)
	require.Len(t, f.Children, 2)
}

func TestRenderRAGPrompt(t *testing.T) {
	got := renderRAGPrompt("Context:\n$search_results$\n\nQ: $query$", "what?", []string{"chunk one", "chunk two"})
	assert.Contains(t, got, "chunk one\n\nchunk two")
	assert.Contains(t, got, "Q: what?")
}

func TestConverseOutputText(t *testing.T) {
	assert.Empty(t, converseOutputText(nil))
	assert.Empty(t, converseOutputText(&bedrockruntime.ConverseOutput{}))

	out := &bedrockruntime.ConverseOutput{
		Output: &bedrockruntime.ConverseOutput_{
			Message: &bedrockruntime.Message{
				Content: []*bedrockruntime.ContentBlock{
					{Text: aws.String("hello ")},
					{Text: aws.String("world")},
				},
			},
		},
	}
	assert.Equal(t, "hello world", converseOutputText(out))
}

func TestQueryResultToRetrievalResult(t *testing.T) {
	r := handlers_ochrevector.QueryResult{Chunk: "some text", SourceKey: "docs/a.txt", Score: 0.87}
	got := queryResultToRetrievalResult(r)
	assert.Equal(t, "some text", *got.Content.Text)
	assert.Equal(t, bedrockagentruntime.RetrievalResultLocationTypeS3, *got.Location.Type)
	assert.Equal(t, "docs/a.txt", *got.Location.S3Location.Uri)
	assert.InDelta(t, 0.87, *got.Score, 0.0001)
}

func newTestKBRecord(t *testing.T, kb *handlers_ochrevector.KBStore, accountID, kbID, indexID string) {
	t.Helper()
	require.NoError(t, kb.Create(context.Background(), accountID, handlers_ochrevector.KBRecord{
		ID: kbID, Name: "docs", Status: handlers_ochrevector.StateReady,
		EmbeddingModel: "amazon.titan-embed-text-v2:0", Dimension: 1024, IndexID: indexID,
	}))
}

func TestRetrieve_MapsRequestAndResponse(t *testing.T) {
	kb, _ := newBedrockAgentTestStores(t)
	newTestKBRecord(t, kb, bedrockAgentTestAccount, "kb-1", "idx-1")

	vector := &fakeBedrockAgentVectorService{queryResp: handlers_ochrevector.QueryResponse{
		Results: []handlers_ochrevector.QueryResult{
			{Chunk: "chunk a", SourceKey: "docs/a.txt", Score: 0.9},
			{Chunk: "chunk b", SourceKey: "docs/b.txt", Score: 0.5},
		},
	}}

	body := []byte(`{
		"knowledgeBaseId": "kb-1",
		"retrievalQuery": {"text": "dragons"},
		"retrievalConfiguration": {"vectorSearchConfiguration": {"numberOfResults": 2, "filter": {"equals": {"key": "genre", "value": "fiction"}}}}
	}`)
	input := &bedrockagentruntime.RetrieveInput{
		KnowledgeBaseId: aws.String("kb-1"),
		RetrievalQuery:  &bedrockagentruntime.KnowledgeBaseQuery{Text: aws.String("dragons")},
		RetrievalConfiguration: &bedrockagentruntime.KnowledgeBaseRetrievalConfiguration{
			VectorSearchConfiguration: &bedrockagentruntime.KnowledgeBaseVectorSearchConfiguration{NumberOfResults: aws.Int64(2)},
		},
	}

	out, err := Retrieve(context.Background(), bedrockAgentTestAccount, kb, vector, body, input)
	require.NoError(t, err)
	require.Len(t, out.RetrievalResults, 2)
	assert.Equal(t, "chunk a", *out.RetrievalResults[0].Content.Text)
	assert.Equal(t, "docs/b.txt", *out.RetrievalResults[1].Location.S3Location.Uri)

	require.NotNil(t, vector.queryReq)
	assert.Equal(t, "idx-1", vector.queryReq.IndexID)
	assert.Equal(t, "dragons", vector.queryReq.Text)
	assert.Equal(t, 2, vector.queryReq.K)
	require.NotNil(t, vector.queryReq.Filter)
	assert.Equal(t, handlers_ochrevector.FilterEquals, vector.queryReq.Filter.Op)
}

func TestRetrieve_UnknownKnowledgeBase(t *testing.T) {
	kb, _ := newBedrockAgentTestStores(t)
	vector := &fakeBedrockAgentVectorService{}
	input := &bedrockagentruntime.RetrieveInput{
		KnowledgeBaseId: aws.String("does-not-exist"),
		RetrievalQuery:  &bedrockagentruntime.KnowledgeBaseQuery{Text: aws.String("dragons")},
	}
	_, err := Retrieve(context.Background(), bedrockAgentTestAccount, kb, vector, nil, input)
	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorResourceNotFoundException))
}

func TestRetrieve_RejectsMissingQueryText(t *testing.T) {
	kb, _ := newBedrockAgentTestStores(t)
	vector := &fakeBedrockAgentVectorService{}
	_, err := Retrieve(context.Background(), bedrockAgentTestAccount, kb, vector, nil, &bedrockagentruntime.RetrieveInput{KnowledgeBaseId: aws.String("kb-1")})
	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorValidationException))
}

// fakeConverse scripts a Converse response so RetrieveAndGenerate can be
// tested without a live model backend, mirroring how fakeBedrockAgentVectorService
// stands in for the daemon.
type fakeConverse struct {
	gotAccountID string
	gotModelID   string
	gotInput     *bedrockruntime.ConverseInput
	resp         *bedrockruntime.ConverseOutput
	err          error
}

func (f *fakeConverse) call(_ context.Context, accountID, modelID string, input *bedrockruntime.ConverseInput) (*bedrockruntime.ConverseOutput, error) {
	f.gotAccountID = accountID
	f.gotModelID = modelID
	f.gotInput = input
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

func fakeConverseOutput(text string) *bedrockruntime.ConverseOutput {
	return &bedrockruntime.ConverseOutput{
		Output: &bedrockruntime.ConverseOutput_{
			Message: &bedrockruntime.Message{Content: []*bedrockruntime.ContentBlock{{Text: aws.String(text)}}},
		},
	}
}

func TestRetrieveAndGenerate_HappyPath(t *testing.T) {
	kb, _ := newBedrockAgentTestStores(t)
	newTestKBRecord(t, kb, bedrockAgentTestAccount, "kb-1", "idx-1")

	vector := &fakeBedrockAgentVectorService{queryResp: handlers_ochrevector.QueryResponse{
		Results: []handlers_ochrevector.QueryResult{{Chunk: "the dragon lives in a cave", SourceKey: "docs/a.txt", Score: 0.9}},
	}}
	fc := &fakeConverse{resp: fakeConverseOutput("The dragon lives in a cave.")}

	input := &bedrockagentruntime.RetrieveAndGenerateInput{
		Input: &bedrockagentruntime.RetrieveAndGenerateInput_{Text: aws.String("where does the dragon live?")},
		RetrieveAndGenerateConfiguration: &bedrockagentruntime.RetrieveAndGenerateConfiguration{
			Type: aws.String(bedrockagentruntime.RetrieveAndGenerateTypeKnowledgeBase),
			KnowledgeBaseConfiguration: &bedrockagentruntime.KnowledgeBaseRetrieveAndGenerateConfiguration{
				KnowledgeBaseId: aws.String("kb-1"),
				ModelArn:        aws.String("arn:aws:bedrock:us-east-1::foundation-model/anthropic.claude-3-5-sonnet-20241022-v2:0"),
			},
		},
	}

	out, err := RetrieveAndGenerate(context.Background(), bedrockAgentTestAccount, kb, vector, fc.call, nil, input)
	require.NoError(t, err)
	assert.Equal(t, "The dragon lives in a cave.", *out.Output.Text)
	require.NotEmpty(t, *out.SessionId)
	require.Len(t, out.Citations, 1)
	require.Len(t, out.Citations[0].RetrievedReferences, 1)
	assert.Equal(t, "the dragon lives in a cave", *out.Citations[0].RetrievedReferences[0].Content.Text)

	assert.Equal(t, "anthropic.claude-3-5-sonnet-20241022-v2:0", fc.gotModelID)
	require.Len(t, fc.gotInput.Messages, 1)
	assert.Equal(t, "where does the dragon live?", *fc.gotInput.Messages[0].Content[0].Text)
	require.Len(t, fc.gotInput.System, 1)
	assert.Contains(t, *fc.gotInput.System[0].Text, "the dragon lives in a cave")
}

func TestRetrieveAndGenerate_EchoesCallerSessionID(t *testing.T) {
	kb, _ := newBedrockAgentTestStores(t)
	newTestKBRecord(t, kb, bedrockAgentTestAccount, "kb-1", "idx-1")
	vector := &fakeBedrockAgentVectorService{}
	fc := &fakeConverse{resp: fakeConverseOutput("answer")}

	input := &bedrockagentruntime.RetrieveAndGenerateInput{
		Input:     &bedrockagentruntime.RetrieveAndGenerateInput_{Text: aws.String("q")},
		SessionId: aws.String("caller-session-id"),
		RetrieveAndGenerateConfiguration: &bedrockagentruntime.RetrieveAndGenerateConfiguration{
			Type: aws.String(bedrockagentruntime.RetrieveAndGenerateTypeKnowledgeBase),
			KnowledgeBaseConfiguration: &bedrockagentruntime.KnowledgeBaseRetrieveAndGenerateConfiguration{
				KnowledgeBaseId: aws.String("kb-1"),
				ModelArn:        aws.String("some-model"),
			},
		},
	}
	out, err := RetrieveAndGenerate(context.Background(), bedrockAgentTestAccount, kb, vector, fc.call, nil, input)
	require.NoError(t, err)
	assert.Equal(t, "caller-session-id", *out.SessionId)
}

func TestRetrieveAndGenerate_UsesCustomPromptTemplate(t *testing.T) {
	kb, _ := newBedrockAgentTestStores(t)
	newTestKBRecord(t, kb, bedrockAgentTestAccount, "kb-1", "idx-1")
	vector := &fakeBedrockAgentVectorService{queryResp: handlers_ochrevector.QueryResponse{
		Results: []handlers_ochrevector.QueryResult{{Chunk: "ctx chunk", SourceKey: "k", Score: 0.5}},
	}}
	fc := &fakeConverse{resp: fakeConverseOutput("answer")}

	input := &bedrockagentruntime.RetrieveAndGenerateInput{
		Input: &bedrockagentruntime.RetrieveAndGenerateInput_{Text: aws.String("my question")},
		RetrieveAndGenerateConfiguration: &bedrockagentruntime.RetrieveAndGenerateConfiguration{
			Type: aws.String(bedrockagentruntime.RetrieveAndGenerateTypeKnowledgeBase),
			KnowledgeBaseConfiguration: &bedrockagentruntime.KnowledgeBaseRetrieveAndGenerateConfiguration{
				KnowledgeBaseId: aws.String("kb-1"),
				ModelArn:        aws.String("some-model"),
				GenerationConfiguration: &bedrockagentruntime.GenerationConfiguration{
					PromptTemplate: &bedrockagentruntime.PromptTemplate{
						TextPromptTemplate: aws.String("CUSTOM: $search_results$ / $query$"),
					},
				},
			},
		},
	}
	_, err := RetrieveAndGenerate(context.Background(), bedrockAgentTestAccount, kb, vector, fc.call, nil, input)
	require.NoError(t, err)
	require.Len(t, fc.gotInput.System, 1)
	assert.Equal(t, "CUSTOM: ctx chunk / my question", *fc.gotInput.System[0].Text)
}

func TestRetrieveAndGenerate_UnknownKnowledgeBase(t *testing.T) {
	kb, _ := newBedrockAgentTestStores(t)
	vector := &fakeBedrockAgentVectorService{}
	fc := &fakeConverse{}
	input := &bedrockagentruntime.RetrieveAndGenerateInput{
		Input: &bedrockagentruntime.RetrieveAndGenerateInput_{Text: aws.String("q")},
		RetrieveAndGenerateConfiguration: &bedrockagentruntime.RetrieveAndGenerateConfiguration{
			Type: aws.String(bedrockagentruntime.RetrieveAndGenerateTypeKnowledgeBase),
			KnowledgeBaseConfiguration: &bedrockagentruntime.KnowledgeBaseRetrieveAndGenerateConfiguration{
				KnowledgeBaseId: aws.String("does-not-exist"),
				ModelArn:        aws.String("some-model"),
			},
		},
	}
	_, err := RetrieveAndGenerate(context.Background(), bedrockAgentTestAccount, kb, vector, fc.call, nil, input)
	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorResourceNotFoundException))
}

func TestRetrieveAndGenerate_RejectsNonKnowledgeBaseType(t *testing.T) {
	kb, _ := newBedrockAgentTestStores(t)
	vector := &fakeBedrockAgentVectorService{}
	fc := &fakeConverse{}
	input := &bedrockagentruntime.RetrieveAndGenerateInput{
		Input: &bedrockagentruntime.RetrieveAndGenerateInput_{Text: aws.String("q")},
		RetrieveAndGenerateConfiguration: &bedrockagentruntime.RetrieveAndGenerateConfiguration{
			Type: aws.String("EXTERNAL_SOURCES"),
		},
	}
	_, err := RetrieveAndGenerate(context.Background(), bedrockAgentTestAccount, kb, vector, fc.call, nil, input)
	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorValidationException))
}

func TestRetrieveAndGenerate_PropagatesConverseError(t *testing.T) {
	kb, _ := newBedrockAgentTestStores(t)
	newTestKBRecord(t, kb, bedrockAgentTestAccount, "kb-1", "idx-1")
	vector := &fakeBedrockAgentVectorService{}
	fc := &fakeConverse{err: errors.New(awserrors.ErrorAccessDeniedException)}
	input := &bedrockagentruntime.RetrieveAndGenerateInput{
		Input: &bedrockagentruntime.RetrieveAndGenerateInput_{Text: aws.String("q")},
		RetrieveAndGenerateConfiguration: &bedrockagentruntime.RetrieveAndGenerateConfiguration{
			Type: aws.String(bedrockagentruntime.RetrieveAndGenerateTypeKnowledgeBase),
			KnowledgeBaseConfiguration: &bedrockagentruntime.KnowledgeBaseRetrieveAndGenerateConfiguration{
				KnowledgeBaseId: aws.String("kb-1"),
				ModelArn:        aws.String("some-model"),
			},
		},
	}
	_, err := RetrieveAndGenerate(context.Background(), bedrockAgentTestAccount, kb, vector, fc.call, nil, input)
	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorAccessDeniedException))
}
