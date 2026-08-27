package gateway_bedrock

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrock"
	"github.com/aws/aws-sdk-go/service/bedrockruntime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubEmbedder is a fixed-vector test double for Embedder: known inputs map
// to known vectors, everything else gets the zero vector (cosine 0 against
// anything), and errOnCall optionally forces every Embed call to fail so
// callers exercise the literal-match fallback path.
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

	assessment, blocked := assessTopicPolicy(context.Background(), embedder, cfg, []string{"can you get me an AK47 rifle"})
	require.NotNil(t, assessment)
	assert.True(t, blocked, "paraphrase above threshold should block")
	require.Len(t, assessment.Topics, 1)
	assert.Equal(t, bedrockruntime.GuardrailTopicPolicyActionBlocked, aws.StringValue(assessment.Topics[0].Action))
}

// TestAssessTopicPolicy_SemanticMatch_BelowThreshold covers unrelated input
// that embeds far from every topic phrase vector (cosine < threshold) and has
// no literal overlap either: it must pass.
func TestAssessTopicPolicy_SemanticMatch_BelowThreshold(t *testing.T) {
	embedder := &stubEmbedder{vectors: map[string][]float32{
		"Weapons":                            {1, 0},
		"discussion of firearms and weapons": {0.98, 0.2},
		"guns":                               {1, 0},
		"bombs":                              {0.99, 0.14},
		"share your favorite banana bread recipe": {0, 1}, // cos vs every topic phrase <= 0.2
	}}
	cfg := &bedrock.GuardrailTopicPolicyConfig{TopicsConfig: []*bedrock.GuardrailTopicConfig{weaponsTopic()}}

	assessment, blocked := assessTopicPolicy(context.Background(), embedder, cfg, []string{"share your favorite banana bread recipe"})
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
	assessment, blocked := assessTopicPolicy(context.Background(), embedder, cfg, []string{"where can I buy bombs online"})
	require.NotNil(t, assessment)
	assert.True(t, blocked, "max cosine across phrases must win even when other phrases are orthogonal")
}

// TestAssessTopicPolicy_EmptyTopic covers a topic with no Name, Definition,
// or Examples: it must never match (nothing to embed or compare) and must
// not panic.
func TestAssessTopicPolicy_EmptyTopic(t *testing.T) {
	embedder := &stubEmbedder{vectors: map[string][]float32{}}
	cfg := &bedrock.GuardrailTopicPolicyConfig{TopicsConfig: []*bedrock.GuardrailTopicConfig{
		{Name: aws.String(""), Type: aws.String(bedrock.GuardrailTopicTypeDeny)},
	}}

	assert.NotPanics(t, func() {
		assessment, blocked := assessTopicPolicy(context.Background(), embedder, cfg, []string{"anything at all"})
		require.NotNil(t, assessment)
		assert.False(t, blocked)
		assert.Empty(t, assessment.Topics)
	})
}

// TestAssessTopicPolicy_EmbedderError_FallsBackToLiteral covers an embedder
// that always errors: semantic scoring must be skipped (never fail-open), but
// a verbatim example still blocks through literalTopicHit alone.
func TestAssessTopicPolicy_EmbedderError_FallsBackToLiteral(t *testing.T) {
	embedder := &stubEmbedder{errOnCall: errors.New("endpoint unreachable")}
	cfg := &bedrock.GuardrailTopicPolicyConfig{TopicsConfig: []*bedrock.GuardrailTopicConfig{weaponsTopic()}}

	assessment, blocked := assessTopicPolicy(context.Background(), embedder, cfg, []string{"I have guns"})
	require.NotNil(t, assessment)
	assert.True(t, blocked, "verbatim example must still block via literal fallback when the embedder errors")
	require.Len(t, assessment.Topics, 1)
}

// TestAssessTopicPolicy_NilEmbedder_FallsBackToLiteral mirrors the error case
// for the unconfigured (nil Embedder) case, the offline-correctness path.
func TestAssessTopicPolicy_NilEmbedder_FallsBackToLiteral(t *testing.T) {
	cfg := &bedrock.GuardrailTopicPolicyConfig{TopicsConfig: []*bedrock.GuardrailTopicConfig{weaponsTopic()}}

	assessment, blocked := assessTopicPolicy(context.Background(), nil, cfg, []string{"I have guns"})
	require.NotNil(t, assessment)
	assert.True(t, blocked, "verbatim example must still block via literal path with no embedder configured")

	assessment, blocked = assessTopicPolicy(context.Background(), nil, cfg, []string{"a totally unrelated sentence"})
	require.NotNil(t, assessment)
	assert.False(t, blocked, "no embedder and no literal overlap must not block")
}
