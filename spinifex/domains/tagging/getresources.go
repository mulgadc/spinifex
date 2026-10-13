package tagging

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	rgt "github.com/aws/aws-sdk-go/service/resourcegroupstaggingapi"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// defaultResourcesPerPage is used when the caller omits ResourcesPerPage (AWS max is 100).
const defaultResourcesPerPage = 100

// Aggregator serves GetResources over the owner listers the composition root
// wires. Listers are called in the order given; it holds no state of its own.
type Aggregator struct {
	listers []TaggedLister
}

// NewAggregator returns an Aggregator that lists from each lister in order.
func NewAggregator(listers ...TaggedLister) *Aggregator {
	return &Aggregator{listers: listers}
}

// GetResources implements the RGT GetResources call, aggregating the account's
// listed resources with type and tag filtering. Pagination uses an opaque
// integer offset into a deterministically ARN-sorted result set.
func (a *Aggregator) GetResources(ctx context.Context, accountID string, body []byte) (any, error) {
	var input rgt.GetResourcesInput
	if err := unmarshalIfBody(body, &input); err != nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	typeFilters := lowerStringSlice(input.ResourceTypeFilters)

	var mappings []*rgt.ResourceTagMapping
	for _, l := range a.listers {
		resources, err := l.ListTagged(ctx, accountID, typeFilters)
		if err != nil {
			return nil, err
		}
		for _, r := range resources {
			mappings = append(mappings, &rgt.ResourceTagMapping{
				ResourceARN: aws.String(r.ARN),
				Tags:        tagMapToRGT(r.Tags),
			})
		}
	}

	mappings = filterByTags(mappings, input.TagFilters)

	// Sort by ARN before pagination.
	sort.Slice(mappings, func(i, j int) bool {
		return aws.StringValue(mappings[i].ResourceARN) < aws.StringValue(mappings[j].ResourceARN)
	})

	page, next, err := paginate(mappings, input.PaginationToken, input.ResourcesPerPage)
	if err != nil {
		return nil, err
	}

	out := &rgt.GetResourcesOutput{ResourceTagMappingList: page}
	out.PaginationToken = aws.String(next) // AWS expects empty string, not nil, on last page
	return out, nil
}

// filterByTags keeps resources matching every TagFilter (AND semantics).
// A non-empty Values list within a filter is an OR; an empty list matches any value.
func filterByTags(mappings []*rgt.ResourceTagMapping, filters []*rgt.TagFilter) []*rgt.ResourceTagMapping {
	if len(filters) == 0 {
		return mappings
	}
	out := mappings[:0]
	for _, m := range mappings {
		if matchesAllTagFilters(m.Tags, filters) {
			out = append(out, m)
		}
	}
	return out
}

func matchesAllTagFilters(tags []*rgt.Tag, filters []*rgt.TagFilter) bool {
	tagMap := make(map[string]string, len(tags))
	for _, t := range tags {
		tagMap[aws.StringValue(t.Key)] = aws.StringValue(t.Value)
	}
	for _, f := range filters {
		key := aws.StringValue(f.Key)
		if key == "" {
			continue
		}
		val, ok := tagMap[key]
		if !ok {
			return false
		}
		if len(f.Values) == 0 {
			continue
		}
		if !slices.ContainsFunc(f.Values, func(v *string) bool {
			return aws.StringValue(v) == val
		}) {
			return false
		}
	}
	return true
}

// paginate slices a page from the sorted mappings and returns the page and the
// next token (empty when exhausted).
func paginate(mappings []*rgt.ResourceTagMapping, token *string, perPage *int64) ([]*rgt.ResourceTagMapping, string, error) {
	start := 0
	if token != nil && *token != "" {
		n, err := strconv.Atoi(*token)
		if err != nil || n < 0 {
			return nil, "", errors.New(awserrors.ErrorInvalidParameterValue)
		}
		start = n
	}
	if start > len(mappings) {
		start = len(mappings)
	}

	size := defaultResourcesPerPage
	if perPage != nil && *perPage > 0 {
		size = int(*perPage)
	}

	end := start + size
	if end >= len(mappings) {
		return mappings[start:], "", nil
	}
	return mappings[start:end], strconv.Itoa(end), nil
}

func tagMapToRGT(tags map[string]string) []*rgt.Tag {
	keys := slices.Sorted(maps.Keys(tags))
	out := make([]*rgt.Tag, 0, len(keys))
	for _, k := range keys {
		out = append(out, &rgt.Tag{Key: aws.String(k), Value: aws.String(tags[k])})
	}
	return out
}

func lowerStringSlice(in []*string) map[string]bool {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]bool, len(in))
	for _, s := range in {
		if s != nil {
			out[strings.ToLower(*s)] = true
		}
	}
	return out
}
