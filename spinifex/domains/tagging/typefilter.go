package tagging

import "strings"

// MatchesType reports whether fullType ("service:type") passes typeFilters.
// An empty filter set admits everything; a service-only filter (e.g. "ec2")
// admits all of that service's types.
func MatchesType(typeFilters map[string]bool, fullType string) bool {
	if len(typeFilters) == 0 {
		return true
	}
	if typeFilters[fullType] {
		return true
	}
	if svc, _, ok := strings.Cut(fullType, ":"); ok {
		if typeFilters[svc] {
			return true
		}
	}
	return false
}
