package gateway_bedrock

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrockruntime"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVLLMProvider_Converse_MapsResponse(t *testing.T) {
	var captured vllmChatRequest

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&captured)) {
			http.Error(w, "decode request body", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"choices": [{"message": {"role": "assistant", "content": "hi there"}, "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 8, "completion_tokens": 3}
		}`))
	}))
	defer ts.Close()

	modelID := "meta.llama3-70b-instruct-v1:0"
	p := newVLLMProvider(NewStaticEndpointResolver(map[string]string{modelID: ts.URL}))
	p.httpClient = ts.Client()

	input := &bedrockruntime.ConverseInput{
		Messages: []*bedrockruntime.Message{
			{Role: aws.String(bedrockruntime.ConversationRoleUser), Content: []*bedrockruntime.ContentBlock{{Text: aws.String("hello")}}},
		},
		InferenceConfig: &bedrockruntime.InferenceConfiguration{MaxTokens: aws.Int64(128)},
	}

	out, err := p.Converse(context.Background(), modelID, input)
	require.NoError(t, err)

	assert.Equal(t, modelID, captured.Model)
	require.NotNil(t, captured.MaxTokens)
	assert.Equal(t, int64(128), *captured.MaxTokens)
	require.Len(t, captured.Messages, 1)
	assert.Equal(t, "user", captured.Messages[0].Role)
	assert.Equal(t, "hello", captured.Messages[0].Content)

	require.NotNil(t, out.Output.Message)
	require.Len(t, out.Output.Message.Content, 1)
	assert.Equal(t, "hi there", *out.Output.Message.Content[0].Text)
	assert.Equal(t, int64(8), *out.Usage.InputTokens)
	assert.Equal(t, int64(3), *out.Usage.OutputTokens)
	assert.Equal(t, int64(11), *out.Usage.TotalTokens)
	assert.Equal(t, bedrockruntime.StopReasonEndTurn, *out.StopReason)
}

func TestVLLMProvider_Converse_UnresolvedEndpointReturnsModelNotReady(t *testing.T) {
	p := newVLLMProvider(NewStaticEndpointResolver(nil))

	input := &bedrockruntime.ConverseInput{
		Messages: []*bedrockruntime.Message{
			{Role: aws.String(bedrockruntime.ConversationRoleUser), Content: []*bedrockruntime.ContentBlock{{Text: aws.String("hello")}}},
		},
	}

	_, err := p.Converse(context.Background(), "meta.llama3-70b-instruct-v1:0", input)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorModelNotReadyException, err.Error())
}
