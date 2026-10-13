package awsgw

import (
	"context"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elbv2"
	ec2tags "github.com/mulgadc/spinifex/spinifex/domains/ec2/tags"
	"github.com/mulgadc/spinifex/spinifex/domains/tagging"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/nats-io/nats.go"
)

// newTaggingAggregator wires GetResources over ELBv2 then the EC2 tag index,
// the order results have always been gathered in.
func newTaggingAggregator(nc *nats.Conn, region string) *tagging.Aggregator {
	return tagging.NewAggregator(newELBv2Lister(nc), newEC2IndexLister(nc, region))
}

// elbv2DescribeTagsBatchSize is the AWS ceiling on ARNs per ELBv2 DescribeTags call.
const elbv2DescribeTagsBatchSize = 20

// ec2IndexLister lists EC2 resources from the central tag index over the
// ec2.DescribeTags subject, building each ARN in the gateway's region.
type ec2IndexLister struct {
	svc    ec2tags.TagsService
	region string
}

var _ tagging.TaggedLister = (*ec2IndexLister)(nil)

func newEC2IndexLister(nc *nats.Conn, region string) *ec2IndexLister {
	return &ec2IndexLister{svc: ec2tags.NewNATSTagsService(nc), region: region}
}

func (l *ec2IndexLister) ListTagged(ctx context.Context, accountID string, typeFilters map[string]bool) ([]tagging.TaggedResource, error) {
	out, err := l.svc.DescribeTags(ctx, &ec2.DescribeTagsInput{}, accountID)
	if err != nil {
		return nil, err
	}
	return ec2TaggedResources(out.Tags, l.region, accountID, typeFilters), nil
}

// ec2TaggedResources groups tag descriptions by resource ID and keeps those
// whose "ec2:<type>" passes typeFilters. The type comes from the first stored
// tag record for the ID, and an empty ID is skipped.
func ec2TaggedResources(tagDescriptions []*ec2.TagDescription, region, accountID string, typeFilters map[string]bool) []tagging.TaggedResource {
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
		rt := byResource[id]
		if rt == nil {
			rt = &resourceTags{fullType: "ec2:" + aws.StringValue(td.ResourceType), tags: map[string]string{}}
			byResource[id] = rt
		}
		rt.tags[aws.StringValue(td.Key)] = aws.StringValue(td.Value)
	}

	var out []tagging.TaggedResource
	for id, rt := range byResource {
		if !tagging.MatchesType(typeFilters, rt.fullType) {
			continue
		}
		out = append(out, tagging.TaggedResource{ARN: ec2ARN(region, accountID, rt.fullType, id), Tags: rt.tags})
	}
	return out
}

// ec2ARN builds an arn:aws:ec2 ARN for a resource. fullType is "ec2:<type>",
// read from the stored tag record rather than from the id.
func ec2ARN(region, accountID, fullType, id string) string {
	kind := arn.EC2ResourceType(strings.TrimPrefix(fullType, "ec2:"))
	return arn.FormatEC2(kind, region, accountID, id)
}

// elbv2Lister lists ELBv2 load balancers and target groups admitted by the
// type filters, tagged or not, with tags from batched DescribeTags calls.
type elbv2Lister struct {
	svc handlers_elbv2.ELBv2Service
}

var _ tagging.TaggedLister = (*elbv2Lister)(nil)

func newELBv2Lister(nc *nats.Conn) *elbv2Lister {
	return &elbv2Lister{svc: handlers_elbv2.NewNATSELBv2Service(nc)}
}

func (l *elbv2Lister) ListTagged(ctx context.Context, accountID string, typeFilters map[string]bool) ([]tagging.TaggedResource, error) {
	wantLB := tagging.MatchesType(typeFilters, "elasticloadbalancing:loadbalancer")
	wantTG := tagging.MatchesType(typeFilters, "elasticloadbalancing:targetgroup")
	if !wantLB && !wantTG {
		return nil, nil
	}

	var arns []string
	if wantLB {
		lbs, err := l.svc.DescribeLoadBalancers(ctx, &elbv2.DescribeLoadBalancersInput{}, accountID)
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
		tgs, err := l.svc.DescribeTargetGroups(ctx, &elbv2.DescribeTargetGroupsInput{}, accountID)
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

	out := make([]tagging.TaggedResource, 0, len(arns))
	for start := 0; start < len(arns); start += elbv2DescribeTagsBatchSize {
		end := min(start+elbv2DescribeTagsBatchSize, len(arns))
		tagsOut, err := l.svc.DescribeTags(ctx, &elbv2.DescribeTagsInput{
			ResourceArns: aws.StringSlice(arns[start:end]),
		}, accountID)
		if err != nil {
			return nil, err
		}
		for _, td := range tagsOut.TagDescriptions {
			tags := make(map[string]string, len(td.Tags))
			for _, t := range td.Tags {
				tags[aws.StringValue(t.Key)] = aws.StringValue(t.Value)
			}
			out = append(out, tagging.TaggedResource{ARN: aws.StringValue(td.ResourceArn), Tags: tags})
		}
	}
	return out, nil
}
