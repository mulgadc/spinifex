// Exercises the unexported chunker internals with no exported surface to
// drive them through.
//
//test:in-package
package handlers_ochrevector

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
