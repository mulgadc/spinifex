package filterutil

import (
	"log/slog"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// ParseFilters converts AWS SDK filter types to map[string][]string.
// tag: prefixed names are always accepted; other names must be in validNames or an error is returned.
func ParseFilters(filters []*ec2.Filter, validNames map[string]bool) (map[string][]string, error) {
	if len(filters) == 0 {
		return nil, nil
	}

	result := make(map[string][]string, len(filters))
	for _, f := range filters {
		if f.Name == nil {
			slog.Warn("ParseFilters: skipping filter with nil Name")
			continue
		}
		name := *f.Name

		// Carries the code itself so callers return it unwrapped; wrapping it
		// again would reach the client stuttered, code repeated in the message.
		if !strings.HasPrefix(name, "tag:") && !validNames[name] {
			return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "The filter '%s' is invalid", name)
		}

		for _, v := range f.Values {
			if v != nil {
				result[name] = append(result[name], *v)
			}
		}
	}
	return result, nil
}

// MatchesAny returns true if value matches any of the filter values.
// Supports the AWS wildcard convention: * matches any substring, ? matches one
// character, and \ escapes either.
// Returns true if filterValues is empty.
func MatchesAny(filterValues []string, value string) bool {
	if len(filterValues) == 0 {
		return true
	}
	return slices.ContainsFunc(filterValues, func(pattern string) bool {
		return MatchWildcard(pattern, value)
	})
}

// IsTagFilter reports whether MatchesTags evaluates the named filter, so a
// resource matcher's switch can skip it.
func IsTagFilter(name string) bool {
	return strings.HasPrefix(name, "tag:") || name == "tag-key" || name == "tag-value"
}

// MatchesTags checks whether a resource's tags satisfy every tag:Key, tag-key
// and tag-value filter in the map. Each filter uses OR logic across its values,
// with wildcard support; tag-key and tag-value match any one of the tags.
func MatchesTags(filters map[string][]string, tags map[string]string) bool {
	for name, values := range filters {
		switch name {
		case "tag-key":
			if !matchesAnyTag(tags, values, func(k, _ string) string { return k }) {
				return false
			}
			continue
		case "tag-value":
			if !matchesAnyTag(tags, values, func(_, v string) string { return v }) {
				return false
			}
			continue
		}
		tagKey, ok := strings.CutPrefix(name, "tag:")
		if !ok {
			continue
		}
		tagValue, exists := tags[tagKey]
		if !exists {
			return false
		}
		if !MatchesAny(values, tagValue) {
			return false
		}
	}
	return true
}

// matchesAnyTag reports whether any tag's selected field (key or value)
// matches any of the filter values.
func matchesAnyTag(tags map[string]string, values []string, field func(k, v string) string) bool {
	for k, v := range tags {
		if MatchesAny(values, field(k, v)) {
			return true
		}
	}
	return false
}

// EC2TagsToMap converts []*ec2.Tag to map[string]string for MatchesTags.
func EC2TagsToMap(tags []*ec2.Tag) map[string]string {
	if len(tags) == 0 {
		return nil
	}
	m := make(map[string]string, len(tags))
	for _, t := range tags {
		if t.Key != nil && t.Value != nil {
			m[*t.Key] = *t.Value
		}
	}
	return m
}

// MatchWildcard matches value against a pattern where * matches zero or more
// characters, ? matches exactly one, and \ escapes the next byte so a literal
// *, ? or \ can still be filtered for. A trailing \ matches itself.
// Case-sensitive; callers needing case-insensitive matching should lower-case both inputs.
//
// Metacharacters are matched over bytes, not runes: EC2 filter values are
// compared bytewise and ? on a multi-byte character has no behaviour to match.
func MatchWildcard(pattern, value string) bool {
	if pattern == "*" {
		return true
	}
	if !strings.ContainsAny(pattern, `*?\`) {
		return pattern == value
	}

	// Greedy scan with a single backtrack point at the most recent "*", which
	// keeps patterns like a*a*a*b linear rather than exponential.
	var p, v int
	star, starV := -1, 0
	for v < len(value) {
		switch {
		case p < len(pattern) && pattern[p] == '*':
			star, starV = p, v
			p++
		case p < len(pattern) && matchesByte(pattern, p, value[v]):
			p += patternWidth(pattern, p)
			v++
		case star >= 0:
			starV++
			p, v = star+1, starV
		default:
			return false
		}
	}

	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

// patternWidth returns the byte length of the pattern token at p: 2 for an
// escape pair, 1 otherwise.
func patternWidth(pattern string, p int) int {
	if pattern[p] == '\\' && p+1 < len(pattern) {
		return 2
	}
	return 1
}

// matchesByte reports whether the single-character pattern token at p matches b.
func matchesByte(pattern string, p int, b byte) bool {
	if pattern[p] == '\\' && p+1 < len(pattern) {
		return pattern[p+1] == b
	}
	return pattern[p] == '?' || pattern[p] == b
}
