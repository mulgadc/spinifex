// Package tagging owns the read-only Resource Groups Tagging aggregation
// capability, composed into AWSGW. Each resource owner keeps its canonical
// tags; this package reads them only through the TaggedLister it declares.
package tagging

import "context"

// TaggedLister returns one owner's resources for GetResources aggregation.
// typeFilters is the lower-cased ResourceTypeFilters set, each entry a full
// "service:type" or a bare service; an empty set admits every type served.
type TaggedLister interface {
	ListTagged(ctx context.Context, accountID string, typeFilters map[string]bool) ([]TaggedResource, error)
}

// TaggedResource is one listed resource; Tags may be empty for an untagged
// resource. Tags is read-only: a lister must not mutate a map it has returned.
type TaggedResource struct {
	ARN  string
	Tags map[string]string
}
