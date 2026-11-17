package vpcpeering

import (
	"context"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	awsfilters "github.com/mulgadc/spinifex/spinifex/foundation/aws/filters"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/paging"
)

var describePaging = paging.EC2{
	MaxResults: 1000,
	TooLarge:   "Value ( %d ) for parameter MaxResults is invalid. Expecting a value smaller than or equal to 1000.",
	TooSmall:   "Value ( %d ) for parameter MaxResults is invalid. Expecting a value greater than or equal to 5.",
	WithIDs:    "The parameter VpcPeeringConnectionIds cannot be used with the parameter MaxResults",
}

var describeValidFilters = map[string]bool{
	"accepter-vpc-info.cidr-block":  true,
	"accepter-vpc-info.owner-id":    true,
	"accepter-vpc-info.vpc-id":      true,
	"expiration-time":               true,
	"requester-vpc-info.cidr-block": true,
	"requester-vpc-info.owner-id":   true,
	"requester-vpc-info.vpc-id":     true,
	"status-code":                   true,
	"status-message":                true,
	"tag-key":                       true,
	"vpc-peering-connection-id":     true,
}

// DescribeVpcPeeringConnections lists the caller's connections from its own
// account index in both buckets, so its cost is the caller's result set.
func (s *ServiceImpl) DescribeVpcPeeringConnections(ctx context.Context, input *ec2.DescribeVpcPeeringConnectionsInput, accountID string) (*ec2.DescribeVpcPeeringConnectionsOutput, error) {
	ids := awsidentifiers.Distinct(input.VpcPeeringConnectionIds)
	pageReq, err := describePaging.Parse(input.MaxResults, input.NextToken, len(ids))
	if err != nil {
		return nil, err
	}
	filters, err := awsfilters.ParseFilters(input.Filters, describeValidFilters)
	if err != nil {
		return nil, err
	}

	views, err := s.viewsFor(ctx, accountID, ids)
	if err != nil {
		return nil, internalError(ctx, "DescribeVpcPeeringConnections", err)
	}
	for _, id := range ids {
		if !slices.ContainsFunc(views, func(v *view) bool { return v.rec.ID == id }) {
			return nil, notFoundError(id)
		}
	}

	var out []*ec2.VpcPeeringConnection
	for _, v := range views {
		if c := viewToEC2(v, accountID); matchesFilters(c, v.rec.Tags[accountID], filters) {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b *ec2.VpcPeeringConnection) int {
		return strings.Compare(*a.VpcPeeringConnectionId, *b.VpcPeeringConnectionId)
	})
	page, next := paging.EC2Page(out, func(c *ec2.VpcPeeringConnection) string { return *c.VpcPeeringConnectionId }, pageReq)
	if page == nil {
		page = []*ec2.VpcPeeringConnection{}
	}
	return &ec2.DescribeVpcPeeringConnectionsOutput{VpcPeeringConnections: page, NextToken: next}, nil
}

// viewsFor returns the connections visible to accountID: the named ones, or
// every one in its account index of either bucket.
func (s *Store) viewsFor(ctx context.Context, accountID string, ids []string) ([]*view, error) {
	if len(ids) == 0 {
		prefix := "acct." + accountID + "."
		live, err := listIndex(ctx, s.live, prefix)
		if err != nil {
			return nil, err
		}
		term, err := listIndex(ctx, s.term, prefix)
		if err != nil {
			return nil, err
		}
		ids = append(live, term...)
		slices.Sort(ids)
		ids = slices.Compact(ids)
	}
	var out []*view
	for _, id := range ids {
		v, err := s.visible(ctx, id, accountID)
		if err != nil {
			return nil, err
		}
		if v != nil {
			out = append(out, v)
		}
	}
	return out, nil
}

// matchesFilters applies the model's filters to a connection as rendered for
// the viewer, so a filter never matches a member the response hides.
func matchesFilters(c *ec2.VpcPeeringConnection, tags map[string]string, filters map[string][]string) bool {
	for name, values := range filters {
		if awsfilters.IsTagFilter(name) {
			continue
		}
		var value string
		switch name {
		case "accepter-vpc-info.cidr-block":
			value = aws.StringValue(c.AccepterVpcInfo.CidrBlock)
		case "accepter-vpc-info.owner-id":
			value = aws.StringValue(c.AccepterVpcInfo.OwnerId)
		case "accepter-vpc-info.vpc-id":
			value = aws.StringValue(c.AccepterVpcInfo.VpcId)
		case "expiration-time":
			if c.ExpirationTime == nil {
				return false
			}
			value = awsTime(*c.ExpirationTime)
		case "requester-vpc-info.cidr-block":
			value = aws.StringValue(c.RequesterVpcInfo.CidrBlock)
		case "requester-vpc-info.owner-id":
			value = aws.StringValue(c.RequesterVpcInfo.OwnerId)
		case "requester-vpc-info.vpc-id":
			value = aws.StringValue(c.RequesterVpcInfo.VpcId)
		case "status-code":
			value = aws.StringValue(c.Status.Code)
		case "status-message":
			value = aws.StringValue(c.Status.Message)
		case "vpc-peering-connection-id":
			value = aws.StringValue(c.VpcPeeringConnectionId)
		}
		if !awsfilters.MatchesAny(values, value) {
			return false
		}
	}
	return awsfilters.MatchesTags(filters, tags)
}
