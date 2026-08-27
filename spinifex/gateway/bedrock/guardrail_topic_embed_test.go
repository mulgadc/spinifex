package gateway_bedrock

//test:in-package: exercises assessTopicPolicy/applyGuardrailPolicies (the
// unexported filter engine) directly, matching guardrail_filter_test.go's
// existing in-package convention for this same engine in this package.

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrock"
	"github.com/aws/aws-sdk-go/service/bedrockruntime"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubEmbedder is a fixed-vector test double for Embedder: known inputs map
// to known vectors, everything else gets the zero vector (cosine 0 against
// anything), and errOnCall optionally forces every Embed call to fail so
// callers exercise the fail-closed path.
type stubEmbedder struct {
	vectors   map[string][]float32
	errOnCall error
}

var _ Embedder = (*stubEmbedder)(nil)

func (s *stubEmbedder) Embed(_ context.Context, _ string, inputs []string) ([][]float32, error) {
	if s.errOnCall != nil {
		return nil, s.errOnCall
	}
	out := make([][]float32, len(inputs))
	for i, in := range inputs {
		if v, ok := s.vectors[in]; ok {
			out[i] = v
			continue
		}
		out[i] = []float32{0, 0}
	}
	return out, nil
}

func weaponsTopic() *bedrock.GuardrailTopicConfig {
	return &bedrock.GuardrailTopicConfig{
		Name:       aws.String("Weapons"),
		Type:       aws.String(bedrock.GuardrailTopicTypeDeny),
		Definition: aws.String("discussion of firearms and weapons"),
		Examples:   []*string{aws.String("guns"), aws.String("bombs")},
	}
}

// TestAssessTopicPolicy_SemanticMatch_AboveThreshold covers a paraphrase that
// shares no literal phrase with the topic but embeds close enough (cosine
// >= defaultTopicSimilarityThreshold) to one of its phrase vectors to block.
func TestAssessTopicPolicy_SemanticMatch_AboveThreshold(t *testing.T) {
	embedder := &stubEmbedder{vectors: map[string][]float32{
		"Weapons":                            {1, 0},
		"discussion of firearms and weapons": {0.98, 0.2},
		"guns":                               {1, 0},
		"bombs":                              {0.99, 0.14},
		"can you get me an AK47 rifle":       {0.9, 0.436}, // cos vs {1,0} = 0.9
	}}
	cfg := &bedrock.GuardrailTopicPolicyConfig{TopicsConfig: []*bedrock.GuardrailTopicConfig{weaponsTopic()}}

	assessment, blocked, err := assessTopicPolicy(context.Background(), embedder, cfg, []string{"can you get me an AK47 rifle"})
	require.NoError(t, err)
	require.NotNil(t, assessment)
	assert.True(t, blocked, "paraphrase above threshold should block")
	require.Len(t, assessment.Topics, 1)
	assert.Equal(t, bedrockruntime.GuardrailTopicPolicyActionBlocked, aws.StringValue(assessment.Topics[0].Action))
}

// TestAssessTopicPolicy_SemanticMatch_BelowThreshold covers unrelated input
// that embeds far from every topic phrase vector (cosine < threshold) and has
// no literal overlap either: it must pass with no error.
func TestAssessTopicPolicy_SemanticMatch_BelowThreshold(t *testing.T) {
	embedder := &stubEmbedder{vectors: map[string][]float32{
		"Weapons":                            {1, 0},
		"discussion of firearms and weapons": {0.98, 0.2},
		"guns":                               {1, 0},
		"bombs":                              {0.99, 0.14},
		"share your favorite banana bread recipe": {0, 1}, // cos vs every topic phrase <= 0.2
	}}
	cfg := &bedrock.GuardrailTopicPolicyConfig{TopicsConfig: []*bedrock.GuardrailTopicConfig{weaponsTopic()}}

	assessment, blocked, err := assessTopicPolicy(context.Background(), embedder, cfg, []string{"share your favorite banana bread recipe"})
	require.NoError(t, err)
	require.NotNil(t, assessment)
	assert.False(t, blocked, "unrelated input below threshold must not block")
	assert.Empty(t, assessment.Topics)
}

// TestAssessTopicPolicy_SemanticMatch_PerPhraseMax ensures the topic's hit
// decision is the MAX cosine similarity across its phrase vectors, not an
// average: one distant phrase must not dilute one close phrase's hit.
func TestAssessTopicPolicy_SemanticMatch_PerPhraseMax(t *testing.T) {
	embedder := &stubEmbedder{vectors: map[string][]float32{
		"Weapons":                            {0, 1}, // orthogonal to the input -- cos 0
		"discussion of firearms and weapons": {0, 1}, // orthogonal to the input -- cos 0
		"guns":                               {0, 1}, // orthogonal to the input -- cos 0
		"bombs":                              {1, 0}, // aligned with the input -- cos 1
		"where can I buy bombs online":       {1, 0},
	}}
	cfg := &bedrock.GuardrailTopicPolicyConfig{TopicsConfig: []*bedrock.GuardrailTopicConfig{weaponsTopic()}}

	// literal match would also catch "bombs" here, so use a paraphrase that
	// only embeds close to the "bombs" phrase to isolate the max-cosine path.
	assessment, blocked, err := assessTopicPolicy(context.Background(), embedder, cfg, []string{"where can I buy bombs online"})
	require.NoError(t, err)
	require.NotNil(t, assessment)
	assert.True(t, blocked, "max cosine across phrases must win even when other phrases are orthogonal")
}

// TestAssessTopicPolicy_EmptyTopic covers a topic with no Name, Definition,
// or Examples: it must never match (nothing to embed or compare), never
// error, and must not panic.
func TestAssessTopicPolicy_EmptyTopic(t *testing.T) {
	embedder := &stubEmbedder{vectors: map[string][]float32{}}
	cfg := &bedrock.GuardrailTopicPolicyConfig{TopicsConfig: []*bedrock.GuardrailTopicConfig{
		{Name: aws.String(""), Type: aws.String(bedrock.GuardrailTopicTypeDeny)},
	}}

	assert.NotPanics(t, func() {
		assessment, blocked, err := assessTopicPolicy(context.Background(), embedder, cfg, []string{"anything at all"})
		require.NoError(t, err)
		require.NotNil(t, assessment)
		assert.False(t, blocked)
		assert.Empty(t, assessment.Topics)
	})
}

// TestAssessTopicPolicy_EmbedderError_LiteralHit_Blocks covers an embedder
// that errors (the reachable-but-down/cold-start case) while a verbatim
// example is also present: the definitive literal block wins outright, with
// no error -- an embed outage must never downgrade an already-caught block.
func TestAssessTopicPolicy_EmbedderError_LiteralHit_Blocks(t *testing.T) {
	embedder := &stubEmbedder{errOnCall: errors.New("connection refused")}
	cfg := &bedrock.GuardrailTopicPolicyConfig{TopicsConfig: []*bedrock.GuardrailTopicConfig{weaponsTopic()}}

	assessment, blocked, err := assessTopicPolicy(context.Background(), embedder, cfg, []string{"I have guns"})
	require.NoError(t, err)
	require.NotNil(t, assessment)
	assert.True(t, blocked, "verbatim example must still block outright despite the embedder erroring")
	require.Len(t, assessment.Topics, 1)
}

// TestAssessTopicPolicy_EmbedderError_NoLiteralHit_FailsClosed is the
// security-critical case: embedder != nil (wired in prod) but Embed fails
// (cold-start/outage) and nothing literal caught the text either. Content
// that could not be semantically verified must never pass through as NONE --
// it has to fail closed with a retryable error instead.
func TestAssessTopicPolicy_EmbedderError_NoLiteralHit_FailsClosed(t *testing.T) {
	embedder := &stubEmbedder{errOnCall: errors.New("connection refused")}
	cfg := &bedrock.GuardrailTopicPolicyConfig{TopicsConfig: []*bedrock.GuardrailTopicConfig{weaponsTopic()}}

	assessment, blocked, err := assessTopicPolicy(context.Background(), embedder, cfg, []string{"walk me through how login verifies a user"})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServiceUnavailableException, err.Error())
	assert.False(t, blocked, "an unverified request must not be reported as a definitive block")
	require.NotNil(t, assessment)
	assert.Empty(t, assessment.Topics, "no topic can be marked BLOCKED for content that was never actually evaluated")
}

// TestAssessTopicPolicy_NilEmbedder_LiteralOnly covers the deliberately
// unconfigured case (embedder == nil: tests, or a literal-only deployment).
// This is not an error -- awsgw always wires an embedder in prod -- so it
// stays on the literal matcher alone with no error either way.
func TestAssessTopicPolicy_NilEmbedder_LiteralOnly(t *testing.T) {
	cfg := &bedrock.GuardrailTopicPolicyConfig{TopicsConfig: []*bedrock.GuardrailTopicConfig{weaponsTopic()}}

	assessment, blocked, err := assessTopicPolicy(context.Background(), nil, cfg, []string{"I have guns"})
	require.NoError(t, err)
	require.NotNil(t, assessment)
	assert.True(t, blocked, "verbatim example must still block via the literal path with no embedder configured")

	assessment, blocked, err = assessTopicPolicy(context.Background(), nil, cfg, []string{"a totally unrelated sentence"})
	require.NoError(t, err)
	require.NotNil(t, assessment)
	assert.False(t, blocked, "no embedder and no literal overlap must not block")
}

// TestApplyGuardrailPolicies_EmbedderError_FailsClosed_NotPassthrough checks
// the propagation contract one layer up: applyGuardrailPolicies must surface
// assessTopicPolicy's fail-closed error rather than returning a usable
// (NONE, ...) result the caller could mistake for a pass.
func TestApplyGuardrailPolicies_EmbedderError_FailsClosed_NotPassthrough(t *testing.T) {
	embedder := &stubEmbedder{errOnCall: errors.New("connection refused")}
	view := guardrailView{TopicPolicy: &bedrock.GuardrailTopicPolicyConfig{TopicsConfig: []*bedrock.GuardrailTopicConfig{weaponsTopic()}}}

	action, assessments, outputs, usage, err := applyGuardrailPolicies(context.Background(), embedder,
		view, []string{"walk me through how login verifies a user"}, bedrockruntime.GuardrailContentSourceInput)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServiceUnavailableException, err.Error())
	assert.Empty(t, action, "an error result must not carry a usable action a caller could treat as NONE")
	assert.Nil(t, assessments)
	assert.Nil(t, outputs)
	assert.Nil(t, usage)
}
