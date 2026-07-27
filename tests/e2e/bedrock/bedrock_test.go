//go:build e2e

package bedrock

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrock"
	"github.com/aws/aws-sdk-go/service/bedrockruntime"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Catalog model IDs mirror the static Phase-1 catalog in gateway_bedrock.
const (
	modelSelfHostLlama = "meta.llama3-70b-instruct-v1:0"
	modelAnthropic     = "anthropic.claude-3-5-sonnet-20240620-v1:0"
	modelBogus         = "does.not-exist-v1:0"
)

// TestGetFoundationModel round-trips a known model and asserts
// ResourceNotFoundException for an unknown one.
func TestGetFoundationModel(t *testing.T) {
	f := requireBedrockFixture(t)

	t.Run("known model", func(t *testing.T) {
		out, err := f.AWS.Bedrock.GetFoundationModel(&bedrock.GetFoundationModelInput{
			ModelIdentifier: aws.String(modelSelfHostLlama),
		})
		require.NoError(t, err, "get-foundation-model %s", modelSelfHostLlama)
		require.NotNil(t, out.ModelDetails, "empty ModelDetails")
		assert.Equal(t, modelSelfHostLlama, aws.StringValue(out.ModelDetails.ModelId))
	})

	t.Run("unknown model", func(t *testing.T) {
		harness.ExpectError(t, "ResourceNotFoundException", func() error {
			_, e := f.AWS.Bedrock.GetFoundationModel(&bedrock.GetFoundationModelInput{
				ModelIdentifier: aws.String(modelBogus),
			})
			return e
		})
	})
}

// TestConverseSelfHost exercises the real vLLM Converse path. Gated behind
// OCHRE_E2E_SELFHOST=1 — it needs a GPU-backed vLLM endpoint configured on the
// gateway via OCHRE_VLLM_ENDPOINTS at boot.
func TestConverseSelfHost(t *testing.T) {
	if os.Getenv("OCHRE_E2E_SELFHOST") != "1" {
		t.Skip("OCHRE_E2E_SELFHOST!=1; skipping real self-host Converse")
	}
	f := requireBedrockFixture(t)

	out, err := f.AWS.BedrockRuntime.Converse(&bedrockruntime.ConverseInput{
		ModelId: aws.String(modelSelfHostLlama),
		Messages: []*bedrockruntime.Message{{
			Role:    aws.String(bedrockruntime.ConversationRoleUser),
			Content: []*bedrockruntime.ContentBlock{{Text: aws.String("Say hello in one word.")}},
		}},
		InferenceConfig: &bedrockruntime.InferenceConfiguration{MaxTokens: aws.Int64(64)},
	})
	require.NoError(t, err, "converse self-host")
	require.NotNil(t, out.Output, "nil output")
	require.NotNil(t, out.Output.Message, "nil output message")
	assert.NotEmpty(t, converseText(out.Output.Message), "empty assistant text")
	require.NotNil(t, out.Usage, "nil usage")
	assert.Greater(t, aws.Int64Value(out.Usage.OutputTokens), int64(0), "OutputTokens must be > 0")
}

// TestConverseAnthropic exercises the real Anthropic-direct Converse path.
// Gated behind OCHRE_E2E_ANTHROPIC=1 — it needs a live Anthropic API key
// configured on the cluster (OCHRE_ANTHROPIC_API_KEY or a per-account KV entry).
func TestConverseAnthropic(t *testing.T) {
	if os.Getenv("OCHRE_E2E_ANTHROPIC") != "1" {
		t.Skip("OCHRE_E2E_ANTHROPIC!=1; skipping real Anthropic Converse")
	}
	f := requireBedrockFixture(t)

	out, err := f.AWS.BedrockRuntime.Converse(&bedrockruntime.ConverseInput{
		ModelId: aws.String(modelAnthropic),
		Messages: []*bedrockruntime.Message{{
			Role:    aws.String(bedrockruntime.ConversationRoleUser),
			Content: []*bedrockruntime.ContentBlock{{Text: aws.String("Say hello in one word.")}},
		}},
		InferenceConfig: &bedrockruntime.InferenceConfiguration{MaxTokens: aws.Int64(64)},
	})
	require.NoError(t, err, "converse anthropic")
	require.NotNil(t, out.Output, "nil output")
	require.NotNil(t, out.Output.Message, "nil output message")
	assert.NotEmpty(t, converseText(out.Output.Message), "empty assistant text")
	require.NotNil(t, out.Usage, "nil usage")
	assert.Greater(t, aws.Int64Value(out.Usage.InputTokens), int64(0), "InputTokens must be > 0")
	assert.Greater(t, aws.Int64Value(out.Usage.OutputTokens), int64(0), "OutputTokens must be > 0")
}

// TestInvokeModelSelfHost exercises the real vLLM InvokeModel path with the
// Bedrock-native Llama request/response shape. Gated behind
// OCHRE_E2E_SELFHOST=1 — it needs a GPU-backed vLLM endpoint configured on the
// gateway via OCHRE_VLLM_ENDPOINTS at boot.
func TestInvokeModelSelfHost(t *testing.T) {
	if os.Getenv("OCHRE_E2E_SELFHOST") != "1" {
		t.Skip("OCHRE_E2E_SELFHOST!=1; skipping real self-host InvokeModel")
	}
	f := requireBedrockFixture(t)

	reqBody, err := json.Marshal(map[string]any{
		"prompt":      "Say hello in one word.",
		"max_gen_len": 64,
	})
	require.NoError(t, err, "marshal invoke-model request")

	out, err := f.AWS.BedrockRuntime.InvokeModel(&bedrockruntime.InvokeModelInput{
		ModelId:     aws.String(modelSelfHostLlama),
		Body:        reqBody,
		ContentType: aws.String("application/json"),
	})
	require.NoError(t, err, "invoke-model self-host")
	assert.Equal(t, "application/json", aws.StringValue(out.ContentType))

	var respBody struct {
		Generation           string `json:"generation"`
		GenerationTokenCount int    `json:"generation_token_count"`
	}
	require.NoError(t, json.Unmarshal(out.Body, &respBody), "unmarshal invoke-model response")
	assert.NotEmpty(t, respBody.Generation, "empty generation")
	assert.Greater(t, respBody.GenerationTokenCount, 0, "GenerationTokenCount must be > 0")
}

// TestInvokeModelAnthropic exercises the real Anthropic-direct InvokeModel
// path with the Bedrock-native Claude Messages request/response shape. Gated
// behind OCHRE_E2E_ANTHROPIC=1 — it needs a live Anthropic API key configured
// on the cluster (OCHRE_ANTHROPIC_API_KEY or a per-account KV entry).
func TestInvokeModelAnthropic(t *testing.T) {
	if os.Getenv("OCHRE_E2E_ANTHROPIC") != "1" {
		t.Skip("OCHRE_E2E_ANTHROPIC!=1; skipping real Anthropic InvokeModel")
	}
	f := requireBedrockFixture(t)

	reqBody, err := json.Marshal(map[string]any{
		"anthropic_version": "bedrock-2023-05-31",
		"max_tokens":        64,
		"messages": []map[string]any{
			{"role": "user", "content": "Say hello in one word."},
		},
	})
	require.NoError(t, err, "marshal invoke-model request")

	out, err := f.AWS.BedrockRuntime.InvokeModel(&bedrockruntime.InvokeModelInput{
		ModelId:     aws.String(modelAnthropic),
		Body:        reqBody,
		ContentType: aws.String("application/json"),
	})
	require.NoError(t, err, "invoke-model anthropic")
	assert.Equal(t, "application/json", aws.StringValue(out.ContentType))

	var respBody struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	require.NoError(t, json.Unmarshal(out.Body, &respBody), "unmarshal invoke-model response")
	require.NotEmpty(t, respBody.Content, "empty content")
	assert.NotEmpty(t, respBody.Content[0].Text, "empty assistant text")
}

// converseText concatenates the text content blocks of a Converse output message.
func converseText(msg *bedrockruntime.Message) string {
	var text string
	for _, c := range msg.Content {
		if c != nil && c.Text != nil {
			text += *c.Text
		}
	}
	return text
}
