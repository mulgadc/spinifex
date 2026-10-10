// Package identifiers provides small helpers for AWS resource identifiers.
package identifiers

// Distinct returns the distinct non-nil values of a request's resource-ID
// slice, in request order. A describe that reads one document per named ID must
// not read or report the same resource twice because the request listed it
// twice.
func Distinct(ids []*string) []string {
	distinct := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == nil || seen[*id] {
			continue
		}
		seen[*id] = true
		distinct = append(distinct, *id)
	}
	return distinct
}
