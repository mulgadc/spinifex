package tgw

import (
	"context"
	"slices"

	"github.com/aws/aws-sdk-go/service/ec2"
	awsfilters "github.com/mulgadc/spinifex/spinifex/foundation/aws/filters"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/paging"
	"github.com/nats-io/nats.go/jetstream"
)

// IDs are point reads and MaxResults with them is accepted, as on AWS, so
// Parse gets no ID count and only range-checks MaxResults and the token.
var describePaging = paging.EC2{
	MaxResults: 1000,
	TooLarge:   "Value ( %d ) for parameter MaxResults is invalid. Expecting a value smaller than or equal to 1000.",
	TooSmall:   "Value ( %d ) for parameter MaxResults is invalid. Expecting a value greater than or equal to 5.",
}

var transitGatewayFilters = map[string]bool{
	"state":              true,
	"tag-key":            true,
	"transit-gateway-id": true,
}

var attachmentFilters = map[string]bool{
	"state":                         true,
	"tag-key":                       true,
	"transit-gateway-attachment-id": true,
	"transit-gateway-id":            true,
	"vpc-id":                        true,
}

// parseFilters answers an unknown name in AWS's Transit Gateway form, which
// differs from the shared parser's.
func parseFilters(in []*ec2.Filter, valid map[string]bool) (map[string][]string, error) {
	for _, f := range in {
		if f == nil || f.Name == nil {
			continue
		}
		if !awsfilters.IsTagFilter(*f.Name) && !valid[*f.Name] {
			return nil, filterInvalid(*f.Name)
		}
	}
	return awsfilters.ParseFilters(in, valid)
}

func matches(filters map[string][]string, tags map[string]string, field func(string) string) bool {
	for name, values := range filters {
		if awsfilters.IsTagFilter(name) {
			continue
		}
		if !awsfilters.MatchesAny(values, field(name)) {
			return false
		}
	}
	return awsfilters.MatchesTags(filters, tags)
}

// accountIDs lists the account's IDs in the live and terminal buckets.
func (s *Store) accountIDs(ctx context.Context, live jetstream.KeyValue, accountID string, want func(string) bool) ([]string, error) {
	prefix := "acct." + accountID + "."
	a, err := listIndex(ctx, live, prefix)
	if err != nil {
		return nil, err
	}
	b, err := listIndex(ctx, s.term, prefix)
	if err != nil {
		return nil, err
	}
	ids := slices.DeleteFunc(append(a, b...), func(id string) bool { return !want(id) })
	slices.Sort(ids)
	return slices.Compact(ids), nil
}

// visibleTransitGateway reads a live record, else a terminal one in its window.
func (s *Store) visibleTransitGateway(ctx context.Context, accountID, id string) (*TransitGatewayRecord, error) {
	rec, _, err := s.transitGateway(ctx, accountID, id)
	if err != nil || rec != nil {
		return rec, err
	}
	return s.terminalTransitGateway(ctx, accountID, id)
}

func (s *Store) visibleAttachment(ctx context.Context, accountID, id string) (*AttachmentRecord, error) {
	rec, _, err := s.attachment(ctx, accountID, id)
	if err != nil || rec != nil {
		return rec, err
	}
	return s.terminalAttachment(ctx, accountID, id)
}

// DescribeTransitGateways reads the named Transit Gateways, or the caller's
// account index in both buckets, so its cost is the caller's inventory.
func (s *ServiceImpl) DescribeTransitGateways(ctx context.Context, input *ec2.DescribeTransitGatewaysInput, accountID string) (*ec2.DescribeTransitGatewaysOutput, error) {
	const op = "DescribeTransitGateways"
	if err := s.readable(ctx, op); err != nil {
		return nil, err
	}
	ids := awsidentifiers.Distinct(input.TransitGatewayIds)
	for _, id := range ids {
		if err := validTransitGatewayID(id); err != nil {
			return nil, err
		}
	}
	pageReq, err := describePaging.Parse(input.MaxResults, input.NextToken, 0)
	if err != nil {
		return nil, err
	}
	filters, err := parseFilters(input.Filters, transitGatewayFilters)
	if err != nil {
		return nil, err
	}
	named := len(ids) > 0
	if !named {
		if ids, err = s.accountIDs(ctx, s.tgws, accountID, isTransitGatewayID); err != nil {
			return nil, internalError(ctx, op, err)
		}
	}
	out := []*ec2.TransitGateway{}
	for _, id := range ids {
		rec, err := s.visibleTransitGateway(ctx, accountID, id)
		if err != nil {
			return nil, internalError(ctx, op, err)
		}
		if rec == nil {
			if named {
				return nil, transitGatewayNotFound(id)
			}
			continue
		}
		if matches(filters, rec.Tags, func(name string) string {
			if name == "state" {
				return rec.State
			}
			return rec.ID
		}) {
			out = append(out, s.transitGatewayToEC2(rec, true))
		}
	}
	if named {
		return &ec2.DescribeTransitGatewaysOutput{TransitGateways: out}, nil
	}
	page, next := paging.EC2Page(out, func(t *ec2.TransitGateway) string { return *t.TransitGatewayId }, pageReq)
	if page == nil {
		page = []*ec2.TransitGateway{}
	}
	return &ec2.DescribeTransitGatewaysOutput{TransitGateways: page, NextToken: next}, nil
}

// DescribeTransitGatewayVpcAttachments is DescribeTransitGateways for attachments.
func (s *ServiceImpl) DescribeTransitGatewayVpcAttachments(ctx context.Context, input *ec2.DescribeTransitGatewayVpcAttachmentsInput, accountID string) (*ec2.DescribeTransitGatewayVpcAttachmentsOutput, error) {
	const op = "DescribeTransitGatewayVpcAttachments"
	if err := s.readable(ctx, op); err != nil {
		return nil, err
	}
	ids := awsidentifiers.Distinct(input.TransitGatewayAttachmentIds)
	for _, id := range ids {
		if err := validAttachmentID(id); err != nil {
			return nil, err
		}
	}
	pageReq, err := describePaging.Parse(input.MaxResults, input.NextToken, 0)
	if err != nil {
		return nil, err
	}
	filters, err := parseFilters(input.Filters, attachmentFilters)
	if err != nil {
		return nil, err
	}
	named := len(ids) > 0
	if !named {
		if ids, err = s.accountIDs(ctx, s.atts, accountID, isAttachmentID); err != nil {
			return nil, internalError(ctx, op, err)
		}
	}
	out := []*ec2.TransitGatewayVpcAttachment{}
	for _, id := range ids {
		rec, err := s.visibleAttachment(ctx, accountID, id)
		if err != nil {
			return nil, internalError(ctx, op, err)
		}
		if rec == nil {
			if named {
				return nil, attachmentNotFound(id)
			}
			continue
		}
		if matches(filters, rec.Tags, func(name string) string {
			switch name {
			case "state":
				return rec.State
			case "transit-gateway-id":
				return rec.TransitGatewayID
			case "vpc-id":
				return rec.VpcID
			}
			return rec.ID
		}) {
			out = append(out, attachmentToEC2(rec))
		}
	}
	if named {
		return &ec2.DescribeTransitGatewayVpcAttachmentsOutput{TransitGatewayVpcAttachments: out}, nil
	}
	page, next := paging.EC2Page(out, func(a *ec2.TransitGatewayVpcAttachment) string { return *a.TransitGatewayAttachmentId }, pageReq)
	if page == nil {
		page = []*ec2.TransitGatewayVpcAttachment{}
	}
	return &ec2.DescribeTransitGatewayVpcAttachmentsOutput{TransitGatewayVpcAttachments: page, NextToken: next}, nil
}
