package gateway_bedrock

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"math"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrock"
)

// defaultTopicSimilarityThreshold is the cosine similarity a topic's
// embedded phrases must reach against an input text's embedding to block.
// Conservative starting value calibrated for nomic-embed-text-v1.5 -- retune
// here if live traffic shows the false positive/negative rate drifting.
const defaultTopicSimilarityThreshold = 0.42

// guardrailServiceLabel tags every slog call in this file, so guardrail
// traffic filtered by labels.service in the log sink surfaces the semantic
// (embedder-backed) topic check's own failure cause instead of only the
// generic gateway request wrapper.
const guardrailServiceLabel = "bedrock-guardrail"

// topicSimilarityThreshold returns topic's similarity threshold. AWS's
// GuardrailTopicConfig wire shape has no field to override this per topic,
// so every topic currently gets the same default; this indirection is the
// seam a future per-topic override plugs into.
func topicSimilarityThreshold(_ *bedrock.GuardrailTopicConfig) float64 {
	return defaultTopicSimilarityThreshold
}

// topicVectorCache holds each denied topic's per-phrase embedding vectors,
// keyed by a hash of the embedding model id plus the topic's own phrase set.
// An unchanged topic -- across applies, or shared by the DRAFT and a
// snapshot taken from it -- is embedded at most once per process lifetime; a
// mutated DRAFT invalidates itself automatically via a changed hash.
var topicVectorCache sync.Map // string -> [][]float32

// topicPhrases returns the phrase set embedded for topic: its Name,
// Definition, and every Example, trimmed and deduplicated so an identical
// phrase is never embedded twice.
func topicPhrases(topic *bedrock.GuardrailTopicConfig) []string {
	seen := make(map[string]bool)
	var phrases []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		phrases = append(phrases, s)
	}
	add(aws.StringValue(topic.Name))
	add(aws.StringValue(topic.Definition))
	for _, ex := range topic.Examples {
		add(aws.StringValue(ex))
	}
	return phrases
}

// topicCacheKey hashes modelID together with phrases, so vectors for a
// different embedding model, or a topic whose phrase set changed, never
// collide in topicVectorCache.
func topicCacheKey(modelID string, phrases []string) string {
	h := sha256.New()
	h.Write([]byte(modelID))
	for _, p := range phrases {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// topicVectors returns topic's cached per-phrase embedding vectors,
// embedding and caching them on first use. A nil, nil return means embedder
// is nil or topic has no phrases -- the caller relies on the literal matcher
// alone, not an error. A non-nil error means embedder was available but the
// embed call itself failed: the caller must fail closed, not fall back.
func topicVectors(ctx context.Context, embedder Embedder, modelID string, topic *bedrock.GuardrailTopicConfig) ([][]float32, error) {
	if embedder == nil || topic == nil {
		return nil, nil
	}
	phrases := topicPhrases(topic)
	if len(phrases) == 0 {
		return nil, nil
	}

	key := topicCacheKey(modelID, phrases)
	if cached, ok := topicVectorCache.Load(key); ok {
		vectors, _ := cached.([][]float32)
		return vectors, nil
	}

	vectors, err := embedder.Embed(ctx, modelID, phrases)
	if err != nil {
		slog.Warn("guardrail: topic embedding unavailable, failing closed on unverified content",
			"service", guardrailServiceLabel, "action", "topic_vectors", "topic", aws.StringValue(topic.Name), "model", modelID, "err", err)
		return nil, err
	}
	topicVectorCache.Store(key, vectors)
	return vectors, nil
}

// cosineSimilarity returns the cosine similarity of a and b. A length
// mismatch or a zero-magnitude vector returns 0 rather than NaN, so a
// malformed embedding never compares true against a positive threshold.
func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, magA, magB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		magA += float64(a[i]) * float64(a[i])
		magB += float64(b[i]) * float64(b[i])
	}
	if magA == 0 || magB == 0 {
		return 0
	}
	return dot / (math.Sqrt(magA) * math.Sqrt(magB))
}

// maxCosineSimilarity returns the highest cosine similarity between vector
// and any vector in candidates, or 0 for an empty candidate set.
func maxCosineSimilarity(vector []float32, candidates [][]float32) float64 {
	best := 0.0
	for _, c := range candidates {
		if s := cosineSimilarity(vector, c); s > best {
			best = s
		}
	}
	return best
}

// embedGuardrailTexts embeds texts once against modelID so every topic in a
// policy reuses the same input vectors instead of re-embedding per topic. A
// nil, nil return means embedder is nil -- the caller relies on the literal
// matcher alone, which is not an error, just an unconfigured deployment. A
// non-nil error means embedder was available but the call failed: the
// caller must fail closed rather than silently pass unverified content.
func embedGuardrailTexts(ctx context.Context, embedder Embedder, modelID string, texts []string) ([][]float32, error) {
	if embedder == nil {
		slog.Warn("guardrail: no embedder configured, falling back to literal topic matching",
			"service", guardrailServiceLabel, "action", "embed_texts")
		return nil, nil
	}
	vectors, err := embedder.Embed(ctx, modelID, texts)
	if err != nil {
		slog.Warn("guardrail: input embedding unavailable, failing closed on unverified content",
			"service", guardrailServiceLabel, "action", "embed_texts", "model", modelID, "err", err)
		return nil, err
	}
	return vectors, nil
}

// topicSemanticHit reports whether any textVector reaches threshold cosine
// similarity against any of topicPhraseVectors -- max-cosine per phrase,
// not a single centroid, so one strong phrase match is enough even when the
// topic's examples are otherwise diverse.
func topicSemanticHit(textVectors, topicPhraseVectors [][]float32, threshold float64) bool {
	for _, tv := range textVectors {
		if maxCosineSimilarity(tv, topicPhraseVectors) >= threshold {
			return true
		}
	}
	return false
}
