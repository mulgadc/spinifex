package handlers_ochrevector

import (
	"context"
	"log/slog"
)

// Reranker is the local seam for cross-encoder reranking: the same method
// set as gateway_bedrock.Reranker, restated here so this package never
// depends on the gateway (mirrors Embedder/TokenCounter).
type Reranker interface {
	Rerank(ctx context.Context, query string, docs []string) ([]int, error)
}

const (
	// rerankOverFetchFactor and rerankMaxCandidates bound the extra
	// candidates Query pulls from the backend before reranking, so a large
	// requested k never turns into an unbounded fetch.
	rerankOverFetchFactor = 4
	rerankMaxCandidates   = 20

	// rerankMaxDocRunes caps the candidate text sent to the reranker per
	// document, bounding the /rerank request body regardless of chunk size.
	rerankMaxDocRunes = 2000
)

// rerankFetchK scales k up by rerankOverFetchFactor for headroom, capped at
// rerankMaxCandidates but never less than k itself.
func rerankFetchK(k int) int {
	return max(k, min(k*rerankOverFetchFactor, rerankMaxCandidates))
}

// rerankTopK reorders results against query and truncates to k. Any
// failure (nil reranker, Rerank error, malformed index) falls back to
// results' existing KNN order truncated to k.
func rerankTopK(ctx context.Context, reranker Reranker, query string, results []QueryResult, k int) []QueryResult {
	knnFallback := func() []QueryResult {
		if len(results) > k {
			return results[:k]
		}
		return results
	}
	if reranker == nil || len(results) == 0 {
		return knnFallback()
	}

	docs := make([]string, len(results))
	for i, r := range results {
		docs[i] = truncateRunes(r.Chunk, rerankMaxDocRunes)
	}

	order, err := reranker.Rerank(ctx, query, docs)
	if err != nil {
		slog.DebugContext(ctx, "ochrevector: rerank failed, falling back to KNN order", "err", err)
		return knnFallback()
	}
	if len(order) > k {
		order = order[:k]
	}

	reranked := make([]QueryResult, 0, len(order))
	for _, idx := range order {
		if idx < 0 || idx >= len(results) {
			slog.DebugContext(ctx, "ochrevector: rerank returned an out-of-range index, falling back to KNN order")
			return knnFallback()
		}
		reranked = append(reranked, results[idx])
	}
	return reranked
}

// truncateRunes bounds s to at most n runes, without the truncation marker
// truncateChunkForResponse appends -- that marker text has no business
// being scored by a cross-encoder as part of the candidate's content.
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
