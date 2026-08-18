package handlers_ochrevector

import (
	"context"
	"log/slog"
	"math"
	"strings"
	"unicode/utf8"
)

// approxCharsPerToken is the documented, deliberately simple chars/token
// ratio behind DefaultChunkSize/DefaultChunkOverlap. There is no true
// tokenizer here -- sizing is in RUNES, not model tokens -- so both defaults
// are an approximation; ChunkTextForModel is the tokenizer-aware entry point
// that replaces this guess with the served embedder's real budget.
const approxCharsPerToken = 4

// DefaultChunkSize and DefaultChunkOverlap approximate ~512 tokens and ~64
// overlap tokens (D10) in runes, via approxCharsPerToken. They remain
// ChunkText's own rune-domain fallback for direct callers; ChunkTextForModel
// never relies on them.
const (
	DefaultChunkSize    = 512 * approxCharsPerToken
	DefaultChunkOverlap = 64 * approxCharsPerToken
)

// codeCharsPerToken is a conservative, worst-case chars-per-token ratio for
// source code, used by ChunkTextForModel to size chunks against a real token
// budget without a live tokenizer. Dense code tokenizes as low as ~2.6-2.8
// chars/token; this sits below that floor deliberately, so a chunk sized
// against it undershoots the real token budget rather than risking a 413.
const codeCharsPerToken = 2.5

// chunkSafetyMargin shrinks the advertised token budget before sizing
// chunks, absorbing BPE merge variance and any special tokens ([CLS]/[SEP])
// the embedder prepends/appends that a raw max_input_length doesn't budget
// for.
const chunkSafetyMargin = 0.9

// DefaultMaxInputTokens is the token budget ChunkTextForModel falls back to
// when the caller has no embedder-advertised max_input_length at all --
// bge-base-en-v1.5's 512-token cap, Ochre's original served embedder.
const DefaultMaxInputTokens = 512

// DefaultChunkOverlapTokens is the token overlap ChunkTextForModel falls
// back to absent an operator override.
const DefaultChunkOverlapTokens = 64

// maxResplitDepth bounds resplitChunk's recursion: verification keeps
// halving a chunk that still exceeds budget, but a pathological input (or a
// TokenCounter that never agrees a chunk is small enough) must not recurse
// forever.
const maxResplitDepth = 8

// minResplitRunes floors resplitChunk's recursion: below this there is
// nothing meaningful left to split, so a chunk still measuring over budget
// here is returned as-is rather than looping.
const minResplitRunes = 8

// chunkSeparators is the recursive-split hierarchy (D10): paragraph, line,
// sentence (three terminators), then word. A span still over size after
// every separator level falls back to a hard rune-count cut.
var chunkSeparators = []string{"\n\n", "\n", ". ", "! ", "? ", " "}

// Chunk is one piece of split document text: Text is the exact substring
// (including any overlap carried over from the previous chunk), Offset is
// its starting position in the original text, measured in runes.
type Chunk struct {
	Text   string
	Offset int
}

// piece is one leaf-level split of the original text, before packing into
// size-bounded, overlapping Chunks.
type piece struct {
	text   string
	offset int
}

// ChunkText splits text into overlapping pieces of at most size runes each,
// recursing over chunkSeparators (paragraph -> line -> sentence -> word) so
// splits prefer natural boundaries over a mid-word or mid-sentence cut; any
// span with no such boundary within size (e.g. one long unbroken token) is
// hard-split by rune count. overlap runes from the tail of one chunk are
// carried into the start of the next, so adjacent chunks share context.
//
// size<=0 defaults to DefaultChunkSize; overlap<0 clamps to 0; overlap>=size
// clamps to size-1, since an overlap consuming (or exceeding) a whole chunk
// would stall the packer's forward progress. Whitespace-only or empty text
// returns nil.
func ChunkText(text string, size, overlap int) []Chunk {
	if size <= 0 {
		size = DefaultChunkSize
	}
	if overlap < 0 {
		overlap = 0
	}
	if overlap >= size {
		overlap = size - 1
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}

	pieces := splitPieces(text, 0, size, overlap, 0)
	return packChunks(pieces, size, overlap)
}

// splitPieces recursively splits text on chunkSeparators[sepIdx:], returning
// the smallest number of ordered, non-overlapping pieces such that every
// piece is at most size runes -- preferring the coarsest separator level
// that achieves that. base is text's rune offset within the original
// document, so every returned piece's offset is correct with no further
// bookkeeping by the caller. overlap is threaded through only to size the
// hardSplit fallback's granularity (see hardSplit).
func splitPieces(text string, base, size, overlap, sepIdx int) []piece {
	if utf8.RuneCountInString(text) <= size {
		return []piece{{text: text, offset: base}}
	}
	if sepIdx >= len(chunkSeparators) {
		return hardSplit(text, base, size, overlap)
	}

	sep := chunkSeparators[sepIdx]
	parts := strings.Split(text, sep)
	if len(parts) == 1 {
		// sep does not occur in text at all; skip straight to the next level
		// rather than looping on a split that changes nothing.
		return splitPieces(text, base, size, overlap, sepIdx+1)
	}

	var out []piece
	cursor := base
	for i, part := range parts {
		seg := part
		if i < len(parts)-1 {
			seg += sep
		}
		segLen := utf8.RuneCountInString(seg)
		if segLen == 0 {
			continue
		}
		if segLen > size {
			out = append(out, splitPieces(seg, cursor, size, overlap, sepIdx+1)...)
		} else {
			out = append(out, piece{text: seg, offset: cursor})
		}
		cursor += segLen
	}
	return out
}

// hardSplit cuts text into contiguous rune spans, the terminal fallback once
// every separator level has been exhausted (e.g. one long unbroken token).
// Granularity is overlap when overlap>0, not size: pieces exactly size runes
// wide would leave packChunks nothing finer than a whole piece to back up
// by, so a requested overlap much smaller than size (the common case) would
// overshoot to a full extra piece. Sizing pieces to overlap instead lets
// packChunks land on (close to) the requested overlap even with no natural
// separator to split on.
func hardSplit(text string, base, size, overlap int) []piece {
	granularity := size
	if overlap > 0 {
		granularity = overlap
	}
	runes := []rune(text)
	out := make([]piece, 0, (len(runes)+granularity-1)/granularity)
	for start := 0; start < len(runes); start += granularity {
		end := min(start+granularity, len(runes))
		out = append(out, piece{text: string(runes[start:end]), offset: base + start})
	}
	return out
}

// packChunks greedily packs ordered, size-bounded pieces into Chunks near
// size runes each, backing up by ~overlap runes between adjacent chunks so
// they share trailing/leading context. Pieces are assumed contiguous and in
// original-text order (splitPieces' guarantee), so Chunk offsets come
// straight from the first piece in each pack and are strictly increasing.
func packChunks(pieces []piece, size, overlap int) []Chunk {
	if len(pieces) == 0 {
		return nil
	}

	var chunks []Chunk
	i := 0
	for i < len(pieces) {
		start := i
		length := 0
		j := i
		for j < len(pieces) {
			pl := utf8.RuneCountInString(pieces[j].text)
			if length > 0 && length+pl > size {
				break
			}
			length += pl
			j++
		}
		if j == start {
			j = start + 1 // always include at least one piece, even if it alone exceeds size
		}

		var sb strings.Builder
		for _, p := range pieces[start:j] {
			sb.WriteString(p.text)
		}
		chunks = append(chunks, Chunk{Text: sb.String(), Offset: pieces[start].offset})

		if j >= len(pieces) {
			break
		}

		// Back up from j toward start to carry ~overlap runes of trailing
		// pieces into the next chunk. k is floored at start+1 so the packer
		// always advances -- even when overlap >= this chunk's whole span.
		k := j
		carried := 0
		for k > start+1 && carried < overlap {
			carried += utf8.RuneCountInString(pieces[k-1].text)
			k--
		}
		i = k
	}
	return chunks
}

// TokenCounter counts text's real token count against a specific served
// model, e.g. via TEI POST /tokenize. ok is false when the endpoint can't or
// didn't answer, telling the caller to fall back to a conservative estimate
// instead of trusting a wrong count. gateway_bedrock's TokenLimiter
// satisfies this structurally, the same way ingest's Embedder mirrors
// gateway_bedrock.Embedder, so this package never imports gateway_bedrock.
type TokenCounter interface {
	CountTokens(ctx context.Context, modelID, text string) (count int, ok bool)
}

// ChunkTextForModel sizes chunks against maxInputTokens -- the served
// embedder's real max_input_length (from TEI /info), clamped by any operator
// override -- rather than a fixed rune guess. It first splits at a rune size
// derived from a conservative chars/token ratio the same way ChunkText's
// caller used to, then verifies every resulting chunk's real token count via
// counter and recursively re-splits any chunk still over budget.
//
// This is the guarantee rune sizing alone cannot make: a separator-less
// dense code span can tokenize denser than any fixed chars/token guess, and
// only a real per-chunk check catches that before it reaches the embedder.
// counter may be nil (embedder doesn't support /tokenize) -- every chunk
// then relies solely on the initial conservative rune sizing, which by
// construction never exceeds maxInputTokens*chunkSafetyMargin as long as
// codeCharsPerToken is a true floor on the served tokenizer's density.
func ChunkTextForModel(ctx context.Context, text, modelID string, maxInputTokens, overlapTokens int, counter TokenCounter) []Chunk {
	if maxInputTokens <= 0 {
		maxInputTokens = DefaultMaxInputTokens
	}
	if overlapTokens < 0 {
		overlapTokens = DefaultChunkOverlapTokens
	}

	budget := max(int(float64(maxInputTokens)*chunkSafetyMargin), 1)
	size := int(float64(budget) * codeCharsPerToken)
	overlapRunes := int(float64(overlapTokens) * codeCharsPerToken)

	chunks := ChunkText(text, size, overlapRunes)
	if len(chunks) == 0 {
		return nil
	}

	out := make([]Chunk, 0, len(chunks))
	for _, c := range chunks {
		out = append(out, resplitChunk(ctx, c, modelID, budget, overlapRunes, counter, 0)...)
	}
	return out
}

// resplitChunk verifies c's real token count against budget and, if it still
// exceeds it, re-splits c.Text at a smaller rune size derived from its
// measured token density and recurses -- the belt-and-suspenders check for
// when the initial conservative sizing wasn't conservative enough. depth
// bounds the recursion (maxResplitDepth); a chunk at or below
// minResplitRunes is returned as-is regardless of its measured count, since
// there is nothing left to usefully split.
func resplitChunk(ctx context.Context, c Chunk, modelID string, budget, overlapRunes int, counter TokenCounter, depth int) []Chunk {
	count := estimateTokens(ctx, c.Text, modelID, counter)
	if count <= budget {
		return []Chunk{c}
	}

	runes := utf8.RuneCountInString(c.Text)
	if depth >= maxResplitDepth || runes <= minResplitRunes {
		slog.WarnContext(ctx, "ochrevector: chunk still exceeds token budget after max re-split depth",
			"model", modelID, "budget_tokens", budget, "measured_tokens", count, "runes", runes)
		return []Chunk{c}
	}

	// Measured density (tokens/rune) sizes the retry directly from what this
	// chunk actually measured, rather than guessing again -- and halves the
	// rune target outright if that still wouldn't shrink it, so depth always
	// makes progress even against a degenerate density estimate.
	density := float64(count) / float64(runes)
	newSize := int(float64(budget) / density)
	if newSize >= runes {
		newSize = runes / 2
	}
	newSize = max(newSize, 1)
	subOverlap := overlapRunes
	if subOverlap >= newSize {
		subOverlap = newSize - 1
	}
	subOverlap = max(subOverlap, 0)

	subChunks := ChunkText(c.Text, newSize, subOverlap)
	out := make([]Chunk, 0, len(subChunks))
	for _, sc := range subChunks {
		sc.Offset += c.Offset
		out = append(out, resplitChunk(ctx, sc, modelID, budget, overlapRunes, counter, depth+1)...)
	}
	return out
}

// estimateTokens counts text's real tokens via counter when available,
// falling back to the same conservative codeCharsPerToken ratio
// ChunkTextForModel used to size chunks in the first place when counter is
// nil or misses.
func estimateTokens(ctx context.Context, text, modelID string, counter TokenCounter) int {
	if counter != nil {
		if n, ok := counter.CountTokens(ctx, modelID, text); ok {
			return n
		}
	}
	return int(math.Ceil(float64(utf8.RuneCountInString(text)) / codeCharsPerToken))
}
