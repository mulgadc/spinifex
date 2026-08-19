// Exercises the unexported chunker internals with no exported surface to
// drive them through.
//
//test:in-package
package handlers_ochrevector

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixedDensityCounter simulates a served tokenizer with a fixed tokens/rune
// density, standing in for TEI POST /tokenize without a live endpoint --
// e.g. tokensPerRune > 1/codeCharsPerToken simulates a tokenizer denser than
// ChunkTextForModel's conservative sizing assumed.
type fixedDensityCounter struct {
	tokensPerRune float64
}

func (f fixedDensityCounter) CountTokens(_ context.Context, _, text string) (int, bool) {
	n := max(int(math.Ceil(float64(utf8.RuneCountInString(text))*f.tokensPerRune)), 1)
	return n, true
}

// alwaysOverBudgetCounter reports a token count no chunk can ever satisfy,
// exercising resplitChunk's depth/minResplitRunes safety valve rather than
// its normal convergence.
type alwaysOverBudgetCounter struct{}

func (alwaysOverBudgetCounter) CountTokens(_ context.Context, _, _ string) (int, bool) {
	return 1_000_000, true
}

func TestChunkText_Empty(t *testing.T) {
	assert.Nil(t, ChunkText("", 50, 10))
}

func TestChunkText_WhitespaceOnly(t *testing.T) {
	assert.Nil(t, ChunkText("   \n\t  \n", 50, 10))
}

func TestChunkText_UnderSize(t *testing.T) {
	text := "hello world, this is short"
	chunks := ChunkText(text, 1000, 100)
	require.Len(t, chunks, 1)
	assert.Equal(t, text, chunks[0].Text)
	assert.Equal(t, 0, chunks[0].Offset)
}

func TestChunkText_ExactSize(t *testing.T) {
	text := strings.Repeat("a", 50)
	chunks := ChunkText(text, 50, 0)
	require.Len(t, chunks, 1)
	assert.Equal(t, text, chunks[0].Text)
	assert.Equal(t, 0, chunks[0].Offset)
}

// TestChunkText_OverSizeHardSplitNoOverlap proves the terminal hard-split
// fallback (no separator anywhere in the text) cuts into contiguous,
// size-bounded spans with exactly abutting offsets when overlap is 0.
func TestChunkText_OverSizeHardSplitNoOverlap(t *testing.T) {
	text := strings.Repeat("a", 205)
	chunks := ChunkText(text, 50, 0)
	require.Len(t, chunks, 5)

	wantLens := []int{50, 50, 50, 50, 5}
	wantOffsets := []int{0, 50, 100, 150, 200}
	for i, c := range chunks {
		assert.Equal(t, wantLens[i], utf8.RuneCountInString(c.Text), "chunk %d length", i)
		assert.Equal(t, wantOffsets[i], c.Offset, "chunk %d offset", i)
	}
	// No separators to split on, but the pieces must still reconstruct the
	// original text with no gaps or corruption.
	assert.Equal(t, text, chunks[0].Text+chunks[1].Text+chunks[2].Text+chunks[3].Text+chunks[4].Text)
}

// TestChunkText_OverlapExactOnHardSplit proves the hard-split fallback lands
// on exactly the requested overlap (not a whole extra piece) by sizing its
// granularity to overlap rather than size.
func TestChunkText_OverlapExactOnHardSplit(t *testing.T) {
	// Each rune is distinguishable by position so an exact-overlap claim is
	// actually verifiable, not just plausible.
	var sb strings.Builder
	for i := range 205 {
		sb.WriteByte(byte('a' + i%26))
	}
	text := sb.String()

	chunks := ChunkText(text, 50, 10)
	require.GreaterOrEqual(t, len(chunks), 2)

	for i := 0; i < len(chunks)-1; i++ {
		cur := []rune(chunks[i].Text)
		next := []rune(chunks[i+1].Text)
		require.GreaterOrEqual(t, len(cur), 10)
		tail := string(cur[len(cur)-10:])
		head := string(next[:10])
		assert.Equal(t, tail, head, "chunk %d tail must equal chunk %d head (10-rune overlap)", i, i+1)
	}
}

// TestChunkText_OffsetMonotonicity proves every chunk's Offset strictly
// increases across a variety of separator levels (paragraphs, lines,
// sentences, words, and unbroken text).
func TestChunkText_OffsetMonotonicity(t *testing.T) {
	text := strings.Repeat("Paragraph one.\n\nParagraph two has more words in it. And another sentence! And one more?\n\nline one\nline two\n\n", 5) + strings.Repeat("x", 300)

	chunks := ChunkText(text, 80, 20)
	require.NotEmpty(t, chunks)
	for i := 1; i < len(chunks); i++ {
		assert.Greater(t, chunks[i].Offset, chunks[i-1].Offset, "offsets must strictly increase")
	}
}

// TestChunkText_ParagraphBoundaryPreferred proves a text with clean
// paragraph breaks under size splits along them rather than mid-word.
func TestChunkText_ParagraphBoundaryPreferred(t *testing.T) {
	p1 := strings.Repeat("alpha ", 5) // 30 runes
	p2 := strings.Repeat("beta ", 5)  // 25 runes
	text := p1 + "\n\n" + p2

	chunks := ChunkText(text, 32, 0)
	require.Len(t, chunks, 2)
	assert.Equal(t, p1+"\n\n", chunks[0].Text)
	assert.Equal(t, p2, chunks[1].Text)
	assert.Equal(t, 0, chunks[0].Offset)
	assert.Equal(t, utf8.RuneCountInString(p1+"\n\n"), chunks[1].Offset)
}

// TestChunkText_SentenceAndWordFallback proves a single long line with no
// paragraph/line breaks falls through to sentence, then word, boundaries.
func TestChunkText_SentenceAndWordFallback(t *testing.T) {
	text := "This is sentence one. This is sentence two. This is sentence three. This is sentence four."
	chunks := ChunkText(text, 30, 5)
	require.NotEmpty(t, chunks)

	// Every chunk's text must be an exact substring of the original at its
	// reported offset -- i.e. no corruption from the recursive splitting.
	runes := []rune(text)
	for _, c := range chunks {
		want := string(runes[c.Offset : c.Offset+utf8.RuneCountInString(c.Text)])
		assert.Equal(t, want, c.Text)
	}
}

// TestChunkText_NoSpacesLongWord proves a single token far longer than size
// (no separator matches at any level) still terminates and hard-splits.
func TestChunkText_NoSpacesLongWord(t *testing.T) {
	text := strings.Repeat("x", 1000)
	chunks := ChunkText(text, 100, 20)
	require.NotEmpty(t, chunks)
	for _, c := range chunks {
		assert.LessOrEqual(t, utf8.RuneCountInString(c.Text), 100)
	}
}

// TestChunkText_DefaultsAreApproximationsInRunes documents that the exported
// defaults are rune counts, not real tokenizer output.
func TestChunkText_DefaultsAreApproximationsInRunes(t *testing.T) {
	assert.Equal(t, 512*approxCharsPerToken, DefaultChunkSize)
	assert.Equal(t, 64*approxCharsPerToken, DefaultChunkOverlap)
	assert.Less(t, DefaultChunkOverlap, DefaultChunkSize)
}

// TestChunkText_NegativeAndZeroInputsClamp proves size<=0 and overlap<0 (or
// overlap>=size) are normalized rather than left to misbehave or loop.
func TestChunkText_NegativeAndZeroInputsClamp(t *testing.T) {
	text := strings.Repeat("word ", 50)

	// size<=0 defaults to DefaultChunkSize -- this text is well under it, so
	// it must come back as a single chunk.
	chunks := ChunkText(text, 0, 0)
	require.Len(t, chunks, 1)

	// overlap<0 clamps to 0 rather than erroring.
	chunks = ChunkText(text, 20, -5)
	require.NotEmpty(t, chunks)

	// overlap>=size clamps to size-1 rather than stalling the packer forever.
	chunks = ChunkText(text, 20, 100)
	require.NotEmpty(t, chunks)
}

// TestChunkTextForModel_DefaultsAreTokenCounts documents that
// ChunkTextForModel's defaults, unlike ChunkText's, are real token counts:
// DefaultMaxInputTokens mirrors bge-base-en-v1.5's 512-token cap and
// DefaultChunkOverlapTokens mirrors D10's ~64-token overlap.
func TestChunkTextForModel_DefaultsAreTokenCounts(t *testing.T) {
	assert.Equal(t, 512, DefaultMaxInputTokens)
	assert.Equal(t, 64, DefaultChunkOverlapTokens)
	assert.Less(t, DefaultChunkOverlapTokens, DefaultMaxInputTokens)
}

// TestChunkTextForModel_ZeroAndNegativeInputsDefault proves maxInputTokens<=0
// and overlapTokens<0 fall back to DefaultMaxInputTokens/
// DefaultChunkOverlapTokens rather than misbehaving, mirroring ChunkText's
// own clamping contract.
func TestChunkTextForModel_ZeroAndNegativeInputsDefault(t *testing.T) {
	text := "hello world, this is a short document"
	chunks := ChunkTextForModel(context.Background(), text, "test-model", 0, -1, nil)
	require.Len(t, chunks, 1)
	assert.Equal(t, text, chunks[0].Text)
}

// TestChunkTextForModel_EmptyTextReturnsNil mirrors ChunkText's empty/
// whitespace-only contract through the token-aware entry point.
func TestChunkTextForModel_EmptyTextReturnsNil(t *testing.T) {
	assert.Nil(t, ChunkTextForModel(context.Background(), "   \n\t  ", "test-model", 100, 10, nil))
}

// TestChunkTextForModel_NilCounterNeverExceedsConservativeEstimate proves
// that even with no TokenCounter at all (an embedder that doesn't expose
// TEI /tokenize), every chunk ChunkTextForModel produces still measures
// under budget by the same conservative codeCharsPerToken estimate the
// sizing pass used -- the guarantee that holds with zero live tokenizer
// calls, as long as codeCharsPerToken is a true floor on real density.
func TestChunkTextForModel_NilCounterNeverExceedsConservativeEstimate(t *testing.T) {
	// No separators anywhere, forcing the hard-split fallback exactly like
	// a long unbroken span of dense code (e.g. a minified line or a long
	// identifier list).
	text := strings.Repeat("x", 5000)
	maxInputTokens := 100
	budget := int(float64(maxInputTokens) * chunkSafetyMargin)

	chunks := ChunkTextForModel(context.Background(), text, "test-model", maxInputTokens, 0, nil)
	require.NotEmpty(t, chunks)
	for _, c := range chunks {
		estimate := int(math.Ceil(float64(utf8.RuneCountInString(c.Text)) / codeCharsPerToken))
		assert.LessOrEqual(t, estimate, budget, "chunk %q exceeds the conservative token estimate", c.Text)
	}
}

// TestChunkTextForModel_ReSplitsChunkThatMeasuresOverLimit is the
// acceptance-bar test: a separator-less dense span that the initial
// conservative rune sizing alone would let through over budget (because the
// mocked served tokenizer is denser than codeCharsPerToken assumed) must be
// caught and re-split by the real-count verification pass, so every final
// chunk measures at or under the model's real token budget.
func TestChunkTextForModel_ReSplitsChunkThatMeasuresOverLimit(t *testing.T) {
	// tokensPerRune=0.5 (2 chars/token) is denser than codeCharsPerToken
	// (2.5), so the initial size==budget*codeCharsPerToken pass alone would
	// leave every chunk measuring ~1.25x over budget without the resplit.
	counter := fixedDensityCounter{tokensPerRune: 0.5}
	maxInputTokens := 100
	budget := int(float64(maxInputTokens) * chunkSafetyMargin)

	text := strings.Repeat("x", 3000) // no separators: forces hardSplit
	chunks := ChunkTextForModel(context.Background(), text, "test-model", maxInputTokens, 0, counter)
	require.NotEmpty(t, chunks)

	for _, c := range chunks {
		count, ok := counter.CountTokens(context.Background(), "test-model", c.Text)
		require.True(t, ok)
		assert.LessOrEqual(t, count, budget, "chunk %q still exceeds the real token budget after re-split", c.Text)
	}

	// Every chunk must still be an exact substring of the original text at
	// its reported offset -- resplitting must not corrupt content.
	runes := []rune(text)
	for _, c := range chunks {
		want := string(runes[c.Offset : c.Offset+utf8.RuneCountInString(c.Text)])
		assert.Equal(t, want, c.Text)
	}
}

// TestChunkTextForModel_PathologicalCounterTerminates proves resplitChunk's
// depth/minResplitRunes safety valve bounds recursion even against a
// TokenCounter that never agrees a chunk is small enough -- it must return
// rather than recurse forever or blow up memory.
func TestChunkTextForModel_PathologicalCounterTerminates(t *testing.T) {
	text := strings.Repeat("y", 12)

	done := make(chan []Chunk, 1)
	go func() {
		done <- ChunkTextForModel(context.Background(), text, "test-model", 100, 0, alwaysOverBudgetCounter{})
	}()

	select {
	case chunks := <-done:
		require.NotEmpty(t, chunks)
		for _, c := range chunks {
			assert.LessOrEqual(t, utf8.RuneCountInString(c.Text), minResplitRunes)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ChunkTextForModel did not terminate against a pathological TokenCounter")
	}
}

// TestChunkTextForModel_CounterConsultedOnlyWhenProvided proves a nil
// counter never panics (estimateTokens falls back to the conservative
// ratio) and that a non-nil counter is actually consulted.
func TestChunkTextForModel_CounterConsultedOnlyWhenProvided(t *testing.T) {
	text := "short text well under any budget"

	assert.NotPanics(t, func() {
		ChunkTextForModel(context.Background(), text, "test-model", 512, 64, nil)
	})

	calls := 0
	counter := countingCounter{fn: func(string) (int, bool) {
		calls++
		return 1, true
	}}
	chunks := ChunkTextForModel(context.Background(), text, "test-model", 512, 64, counter)
	require.NotEmpty(t, chunks)
	assert.Positive(t, calls, "a non-nil counter must be consulted for verification")
}

// countingCounter wraps a closure as a TokenCounter, for asserting a
// counter was actually invoked.
type countingCounter struct {
	fn func(text string) (int, bool)
}

func (c countingCounter) CountTokens(_ context.Context, _, text string) (int, bool) {
	return c.fn(text)
}
