package handlers_ochrevector

import (
	"strings"
	"unicode/utf8"
)

// approxCharsPerToken is the documented, deliberately simple chars/token
// ratio behind DefaultChunkSize/DefaultChunkOverlap. There is no true
// tokenizer here -- sizing is in RUNES, not model tokens -- so both defaults
// are an approximation; a per-model tokenizer is a later refinement.
const approxCharsPerToken = 4

// DefaultChunkSize and DefaultChunkOverlap approximate ~512 tokens and ~64
// overlap tokens (D10) in runes, via approxCharsPerToken.
const (
	DefaultChunkSize    = 512 * approxCharsPerToken
	DefaultChunkOverlap = 64 * approxCharsPerToken
)

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
