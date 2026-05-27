package filterutil

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/aws/aws-sdk-go/service/ec2"
)

// ParseFilters converts AWS SDK filter types into a map[string][]string for
// easier matching. validNames defines the set of accepted filter names.
// Filter names starting with "tag:" are always accepted. Returns an
// InvalidParameterValue-style error if an unsupported filter name is
// encountered.
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

		if !strings.HasPrefix(name, "tag:") && !validNames[name] {
			return nil, fmt.Errorf("InvalidParameterValue: The filter '%s' is invalid", name)
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
// Supports the AWS wildcard convention where * matches any substring.
// Returns true if filterValues is empty.
func MatchesAny(filterValues []string, value string) bool {
	if len(filterValues) == 0 {
		return true
	}
	for _, pattern := range filterValues {
		if MatchWildcard(pattern, value) {
			return true
		}
	}
	return false
}

// MatchesTags checks whether a resource's tags satisfy all tag:Key filters
// present in the filter map. Each tag:Key filter uses OR logic across its
// values — the resource must have the tag and its value must match at least
// one filter value (with wildcard support).
func MatchesTags(filters map[string][]string, tags map[string]string) bool {
	for name, values := range filters {
		if !strings.HasPrefix(name, "tag:") {
			continue
		}
		tagKey := name[4:] // strip "tag:" prefix
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
// characters. Case-sensitive — callers needing case-insensitive matching
// (e.g. IAM policy evaluation) should lower-case both inputs before calling.
func MatchWildcard(pattern, value string) bool {
	if pattern == "*" {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return pattern == value
	}

	parts := strings.Split(pattern, "*")
	last := len(parts) - 1

	// Check that value starts with the first part and ends with the last.
	if !strings.HasPrefix(value, parts[0]) {
		return false
	}
	if !strings.HasSuffix(value, parts[last]) {
		return false
	}

	// Trim prefix and suffix from the search region so middle parts
	// cannot consume characters reserved for the anchored ends.
	remaining := value[len(parts[0]):]
	if len(remaining) < len(parts[last]) {
		return false
	}
	remaining = remaining[:len(remaining)-len(parts[last])]

	// Walk through middle parts in order.
	for i := 1; i < last; i++ {
		idx := strings.Index(remaining, parts[i])
		if idx < 0 {
			return false
		}
		remaining = remaining[idx+len(parts[i]):]
	}
	return true
}
