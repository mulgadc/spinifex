package awsmodel

import (
	"regexp"
	"regexp/syntax"
	"strings"
	"sync"
	"unicode/utf8"
)

// maxGeneratedLength bounds generated strings, so a constraint such as a
// 128 KiB policy document maximum does not produce a request that size.
const maxGeneratedLength = 1 << 16

// patternSampler produces strings matching a model pattern. Smithy patterns
// are not implicitly anchored, so a match anywhere in the value satisfies one.
type patternSampler struct {
	re       *regexp.Regexp
	anchored *regexp.Regexp
	syntax   *syntax.Regexp

	mu      sync.Mutex
	samples map[[2]int]sampleResult
}

type sampleResult struct {
	value string
	ok    bool
}

type samplerResult struct {
	sampler *patternSampler
	err     error
}

// samplers caches one sampler per pattern: every request case regenerates
// its input, and the same patterns recur across operations.
var samplers sync.Map

// javaEscape matches the \uXXXX escapes AWS patterns use, which Go spells \x{XXXX}.
var javaEscape = regexp.MustCompile(`\\u([0-9A-Fa-f]{4})`)

func newPatternSampler(pattern string) (*patternSampler, error) {
	if cached, ok := samplers.Load(pattern); ok {
		if result, ok := cached.(samplerResult); ok {
			return result.sampler, result.err
		}
	}
	sampler, err := compilePatternSampler(pattern)
	samplers.Store(pattern, samplerResult{sampler: sampler, err: err})
	return sampler, err
}

func compilePatternSampler(pattern string) (*patternSampler, error) {
	pattern = javaEscape.ReplaceAllString(pattern, `\x{$1}`)
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	parsed, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return nil, err
	}
	anchored, err := regexp.Compile(`^(?:` + pattern + `)$`)
	if err != nil {
		return nil, err
	}
	return &patternSampler{re: re, anchored: anchored, syntax: parsed.Simplify(), samples: map[[2]int]sampleResult{}}, nil
}

// sample returns a string that fully matches the pattern and whose length in
// runes is within [minLength, maxLength]. Every unbounded or ranged repetition
// is expanded the same number of times; the count is searched for, since the
// length only grows with it.
func (p *patternSampler) sample(minLength, maxLength int) (string, bool) {
	key := [2]int{minLength, maxLength}
	p.mu.Lock()
	defer p.mu.Unlock()
	if cached, ok := p.samples[key]; ok {
		return cached.value, cached.ok
	}
	value, ok := p.search(minLength, maxLength)
	p.samples[key] = sampleResult{value: value, ok: ok}
	return value, ok
}

func (p *patternSampler) search(minLength, maxLength int) (string, bool) {
	lengthAt := func(repeat int) (string, int, bool) {
		var builder strings.Builder
		if !writeSample(&builder, p.syntax, repeat, maxLength+1) {
			return "", 0, false
		}
		value := builder.String()
		return value, utf8.RuneCountInString(value), true
	}

	value, length, ok := lengthAt(0)
	if !ok {
		return "", false
	}
	repeat := 0
	if length < minLength {
		// Double until long enough, then bisect for the smallest count.
		low, high := 0, 1
		for {
			_, length, ok = lengthAt(high)
			if !ok {
				return "", false
			}
			if length >= minLength {
				break
			}
			if high >= maxGeneratedLength {
				return "", false
			}
			low, high = high, high*2
		}
		for low+1 < high {
			middle := (low + high) / 2
			if _, length, _ = lengthAt(middle); length >= minLength {
				high = middle
			} else {
				low = middle
			}
		}
		repeat = high
	}
	// A pattern whose fixed parts need a larger count than the minimum does
	// can still fit a few counts higher.
	for attempt := range 4 {
		value, length, ok = lengthAt(repeat + attempt)
		if !ok || length > maxLength {
			return "", false
		}
		if length >= minLength && p.fullMatch(value) {
			return value, true
		}
	}
	return "", false
}

// fullMatch reports whether the whole value matches the pattern. Generated
// values are held to this, stricter than Smithy's unanchored match, so a value
// like "/a" is not taken for a path because it contains a "/".
func (p *patternSampler) fullMatch(value string) bool {
	return p.anchored.MatchString(value)
}

func writeSample(builder *strings.Builder, re *syntax.Regexp, repeat, limit int) bool {
	// Past the limit the value is already too long; the caller only needs to
	// see that.
	if builder.Len() > limit*utf8.UTFMax {
		return true
	}
	switch re.Op {
	case syntax.OpNoMatch:
		return false
	case syntax.OpLiteral:
		for _, r := range re.Rune {
			builder.WriteRune(r)
		}
	case syntax.OpCharClass:
		r, ok := classRune(re.Rune)
		if !ok {
			return false
		}
		builder.WriteRune(r)
	case syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		builder.WriteRune('a')
	case syntax.OpCapture:
		return writeSample(builder, re.Sub[0], repeat, limit)
	case syntax.OpConcat:
		for _, sub := range re.Sub {
			if !writeSample(builder, sub, repeat, limit) {
				return false
			}
		}
	case syntax.OpAlternate:
		for _, sub := range re.Sub {
			var alternative strings.Builder
			if writeSample(&alternative, sub, repeat, limit) {
				builder.WriteString(alternative.String())
				return true
			}
		}
		return false
	case syntax.OpStar, syntax.OpPlus, syntax.OpQuest, syntax.OpRepeat:
		count := repetitions(re, repeat)
		for range count {
			if !writeSample(builder, re.Sub[0], repeat, limit) {
				return false
			}
		}
	}
	// Anchors, word boundaries and empty matches contribute no characters.
	return true
}

func repetitions(re *syntax.Regexp, repeat int) int {
	low, high := 0, -1
	switch re.Op {
	case syntax.OpPlus:
		low = 1
	case syntax.OpQuest:
		high = 1
	case syntax.OpRepeat:
		low, high = re.Min, re.Max
	}
	count := max(repeat, low)
	if high >= 0 {
		count = min(count, high)
	}
	return count
}

// classRune picks a readable member of a character class: a lower-case
// letter, then a digit, then any printable ASCII, then the first rune.
func classRune(ranges []rune) (rune, bool) {
	if len(ranges) == 0 {
		return 0, false
	}
	for _, preferred := range []string{"abcdefghijklmnopqrstuvwxyz", "0123456789", "ABCDEFGHIJKLMNOPQRSTUVWXYZ", "-_.!#$%&'()*+,/:;<=>?@[]^`{|}~"} {
		for _, r := range preferred {
			if inClass(ranges, r) {
				return r, true
			}
		}
	}
	return ranges[0], true
}

func inClass(ranges []rune, r rune) bool {
	for i := 0; i+1 < len(ranges); i += 2 {
		if r >= ranges[i] && r <= ranges[i+1] {
			return true
		}
	}
	return false
}
