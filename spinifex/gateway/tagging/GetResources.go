package gateway_tagging

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elbv2"
	rgt "github.com/aws/aws-sdk-go/service/resourcegroupstaggingapi"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_tags "github.com/mulgadc/spinifex/spinifex/handlers/ec2/tags"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/nats-io/nats.go"
)

// describeTagsBatchSize is the AWS ceiling on ARNs per ELBv2 DescribeTags call.
const describeTagsBatchSize = 20

// defaultResourcesPerPage is used when the caller omits ResourcesPerPage (AWS max is 100).
const defaultResourcesPerPage = 100

// resourceLister fetches an account's tagged resources shaped as RGT mappings,
// split out so filter/sort/paginate logic can be tested without a live NATS backend.
type resourceLister interface {
	listELBv2(typeFilters map[string]bool) ([]*rgt.ResourceTagMapping, error)
	listEC2(typeFilters map[string]bool) ([]*rgt.ResourceTagMapping, error)
}

// GetResources implements the RGT GetResources call, aggregating tagged ELBv2
// and EC2 resources for the account with type and tag filtering. Pagination uses
// an opaque integer offset into a deterministically ARN-sorted result set.
func GetResources(natsConn *nats.Conn, region, accountID string, body []byte) (any, error) {
	return getResources(&natsLister{natsConn: natsConn, region: region, accountID: accountID}, body)
}

// getResources is the protocol-independent core: parses the request, collects
// resource families from the lister, then filters, sorts, and paginates.
func getResources(lister resourceLister, body []byte) (any, error) {
	var input rgt.GetResourcesInput
	if err := unmarshalIfBody(body, &input); err != nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	typeFilters := lowerStringSlice(input.ResourceTypeFilters)

	var mappings []*rgt.ResourceTagMapping

	elbMappings, err := lister.listELBv2(typeFilters)
	if err != nil {
		return nil, err
	}
	mappings = append(mappings, elbMappings...)

	ec2Mappings, err := lister.listEC2(typeFilters)
	if err != nil {
		return nil, err
	}
	mappings = append(mappings, ec2Mappings...)

	mappings = filterByTags(mappings, input.TagFilters)

	// Stable sort so pagination tokens are consistent across pages.
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

// natsLister fetches tagged resources from the elbv2 and ec2 tag stores via NATS.
type natsLister struct {
	natsConn  *nats.Conn
	region    string
	accountID string
}

func (l *natsLister) listELBv2(typeFilters map[string]bool) ([]*rgt.ResourceTagMapping, error) {
	return elbv2Resources(l.natsConn, l.accountID, typeFilters)
}

func (l *natsLister) listEC2(typeFilters map[string]bool) ([]*rgt.ResourceTagMapping, error) {
	svc := handlers_ec2_tags.NewNATSTagsService(l.natsConn)
	tagsOut, err := svc.DescribeTags(&ec2.DescribeTagsInput{}, l.accountID)
	if err != nil {
		return nil, err
	}
	return buildEC2Mappings(tagsOut.Tags, l.region, l.accountID, typeFilters), nil
}

// elbv2Resources lists load balancers and target groups admitted by typeFilters,
// attaching tags via batched DescribeTags.
func elbv2Resources(natsConn *nats.Conn, accountID string, typeFilters map[string]bool) ([]*rgt.ResourceTagMapping, error) {
	wantLB := matchesType(typeFilters, "elasticloadbalancing:loadbalancer")
	wantTG := matchesType(typeFilters, "elasticloadbalancing:targetgroup")
	if !wantLB && !wantTG {
		return nil, nil
	}

	svc := handlers_elbv2.NewNATSELBv2Service(natsConn)
	var arns []string

	if wantLB {
		lbs, err := svc.DescribeLoadBalancers(&elbv2.DescribeLoadBalancersInput{}, accountID)
		if err != nil {
			return nil, err
		}
		for _, lb := range lbs.LoadBalancers {
			if lb.LoadBalancerArn != nil {
				arns = append(arns, *lb.LoadBalancerArn)
			}
		}
	}
	if wantTG {
		tgs, err := svc.DescribeTargetGroups(&elbv2.DescribeTargetGroupsInput{}, accountID)
		if err != nil {
			return nil, err
		}
		for _, tg := range tgs.TargetGroups {
			if tg.TargetGroupArn != nil {
				arns = append(arns, *tg.TargetGroupArn)
			}
		}
	}
	if len(arns) == 0 {
		return nil, nil
	}

	mappings := make([]*rgt.ResourceTagMapping, 0, len(arns))
	for start := 0; start < len(arns); start += describeTagsBatchSize {
		end := min(start+describeTagsBatchSize, len(arns))
		tagsOut, err := svc.DescribeTags(&elbv2.DescribeTagsInput{
			ResourceArns: aws.StringSlice(arns[start:end]),
		}, accountID)
		if err != nil {
			return nil, err
		}
		for _, td := range tagsOut.TagDescriptions {
			mappings = append(mappings, &rgt.ResourceTagMapping{
				ResourceARN: td.ResourceArn,
				Tags:        elbv2TagsToRGT(td.Tags),
			})
		}
	}
	return mappings, nil
}

// buildEC2Mappings groups EC2 tag descriptions by resource, builds an ARN per
// resource, and filters by type. EC2 tags use a bare resource ID and type word
// (e.g. "subnet"); RGT uses "ec2:<type>". Pure function, no NATS dependency.
func buildEC2Mappings(tagDescriptions []*ec2.TagDescription, region, accountID string, typeFilters map[string]bool) []*rgt.ResourceTagMapping {
	type resourceTags struct {
		fullType string
		tags     map[string]string
	}
	byResource := make(map[string]*resourceTags)
	for _, td := range tagDescriptions {
		id := aws.StringValue(td.ResourceId)
		if id == "" {
			continue
		}
		fullType := "ec2:" + aws.StringValue(td.ResourceType)
		rt := byResource[id]
		if rt == nil {
			rt = &resourceTags{fullType: fullType, tags: map[string]string{}}
			byResource[id] = rt
		}
		rt.tags[aws.StringValue(td.Key)] = aws.StringValue(td.Value)
	}

	var mappings []*rgt.ResourceTagMapping
	for id, rt := range byResource {
		if !matchesType(typeFilters, rt.fullType) {
			continue
		}
		mappings = append(mappings, &rgt.ResourceTagMapping{
			ResourceARN: aws.String(ec2ARN(region, accountID, rt.fullType, id)),
			Tags:        tagMapToRGT(rt.tags),
		})
	}
	return mappings
}

// ec2ARN builds an arn:aws:ec2 ARN for a resource. fullType is "ec2:<type>".
func ec2ARN(region, accountID, fullType, id string) string {
	resourceType := strings.TrimPrefix(fullType, "ec2:")
	return fmt.Sprintf("arn:aws:ec2:%s:%s:%s/%s", region, accountID, resourceType, id)
}

// matchesType reports whether fullType passes typeFilters. An empty filter set
// admits everything; a service-only filter (e.g. "ec2") admits all subtypes.
func matchesType(typeFilters map[string]bool, fullType string) bool {
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
		matched := false
		for _, v := range f.Values {
			if aws.StringValue(v) == val {
				matched = true
				break
			}
		}
		if !matched {
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

func elbv2TagsToRGT(tags []*elbv2.Tag) []*rgt.Tag {
	out := make([]*rgt.Tag, 0, len(tags))
	for _, t := range tags {
		out = append(out, &rgt.Tag{Key: t.Key, Value: t.Value})
	}
	return out
}

func tagMapToRGT(tags map[string]string) []*rgt.Tag {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
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
