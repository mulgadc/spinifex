package vpcendpoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsfilters "github.com/mulgadc/spinifex/spinifex/foundation/aws/filters"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/paging"
)

var describePaging = paging.EC2{
	MaxResults: 1000,
	TooLarge:   "Value ( %d ) for parameter maxResults is invalid. Expecting a value less than or equal to 1000.",
	TooSmall:   "Value ( %d ) for parameter maxResults is invalid. Expecting a value greater than or equal to 5.",
	WithIDs:    "The parameter vpcEndpointIds cannot be used with the parameter maxResults",
}

var describeValidFilters = map[string]bool{
	"ip-address-type":    true,
	"service-name":       true,
	"service-region":     true,
	"tag-key":            true,
	"vpc-id":             true,
	"vpc-endpoint-id":    true,
	"vpc-endpoint-state": true,
	"vpc-endpoint-type":  true,
}

var servicesValidFilters = map[string]bool{
	"owner":          true,
	"service-name":   true,
	"service-region": true,
	"service-type":   true,
}

// parseFilters refuses an unknown name with AWS's endpoint wording before
// the shared parser, whose wording differs.
func parseFilters(filters []*ec2.Filter, valid map[string]bool) (map[string][]string, error) {
	for _, f := range filters {
		if name := aws.StringValue(f.Name); !strings.HasPrefix(name, "tag:") && !valid[name] {
			return nil, awserrors.Errorf(awserrors.ErrorInvalidFilter, "Unsupported filter attribute: [%s]", name)
		}
	}
	return awsfilters.ParseFilters(filters, valid)
}

// DescribeVpcEndpoints lists the caller's available endpoints from its own
// account keys, so its cost is the caller's result set.
func (s *ServiceImpl) DescribeVpcEndpoints(ctx context.Context, input *ec2.DescribeVpcEndpointsInput, accountID string) (*ec2.DescribeVpcEndpointsOutput, error) {
	ids := awsidentifiers.Distinct(input.VpcEndpointIds)
	pageReq, err := describePaging.Parse(input.MaxResults, input.NextToken, len(ids))
	if err != nil {
		return nil, err
	}
	// Until activation is installed no endpoint can exist, and the answer
	// stays the empty inventory the route has always returned.
	if s.active == nil {
		return &ec2.DescribeVpcEndpointsOutput{VpcEndpoints: []*ec2.VpcEndpoint{}}, nil
	}
	filters, err := parseFilters(input.Filters, describeValidFilters)
	if err != nil {
		return nil, err
	}

	var recs []*EndpointRecord
	if len(ids) == 0 {
		if recs, err = s.accountRecords(ctx, accountID); err != nil {
			return nil, internalError(ctx, "DescribeVpcEndpoints", err)
		}
	} else {
		for _, id := range ids {
			rec, _, err := s.load(ctx, accountID, id)
			if err != nil {
				return nil, internalError(ctx, "DescribeVpcEndpoints", err)
			}
			if rec == nil || rec.State != StateAvailable {
				return nil, endpointNotFound(id)
			}
			recs = append(recs, rec)
		}
	}

	out := []*ec2.VpcEndpoint{}
	for _, rec := range recs {
		if rec.State != StateAvailable {
			continue
		}
		tables, err := s.effectiveTables(ctx, rec)
		if err != nil {
			return nil, internalError(ctx, "DescribeVpcEndpoints", err)
		}
		if e := recordToEC2(rec, tables); s.matchesFilters(e, rec.Tags, filters) {
			out = append(out, e)
		}
	}
	slices.SortFunc(out, func(a, b *ec2.VpcEndpoint) int { return strings.Compare(*a.VpcEndpointId, *b.VpcEndpointId) })
	page, next := paging.EC2Page(out, func(e *ec2.VpcEndpoint) string { return *e.VpcEndpointId }, pageReq)
	if page == nil {
		page = []*ec2.VpcEndpoint{}
	}
	return &ec2.DescribeVpcEndpointsOutput{VpcEndpoints: page, NextToken: next}, nil
}

func (s *ServiceImpl) matchesFilters(e *ec2.VpcEndpoint, tags map[string]string, filters map[string][]string) bool {
	for name, values := range filters {
		var value string
		switch name {
		case "ip-address-type":
			value = aws.StringValue(e.IpAddressType)
		case "service-name":
			value = aws.StringValue(e.ServiceName)
		case "service-region":
			value = s.settings.Region
		case "vpc-id":
			value = aws.StringValue(e.VpcId)
		case "vpc-endpoint-id":
			value = aws.StringValue(e.VpcEndpointId)
		case "vpc-endpoint-state":
			value = aws.StringValue(e.State)
		case "vpc-endpoint-type":
			value = aws.StringValue(e.VpcEndpointType)
		default:
			continue
		}
		if !awsfilters.MatchesAny(values, value) {
			return false
		}
	}
	return awsfilters.MatchesTags(filters, tags)
}

// recordToEC2 renders rec as AWS renders a gateway endpoint, naming only the
// tables whose association is effective.
func recordToEC2(rec *EndpointRecord, tables []string) *ec2.VpcEndpoint {
	policy := rec.PolicyDocument
	if policy == "" {
		policy = DefaultPolicy
	}
	dnsType := rec.DnsRecordIpType
	if dnsType == "" {
		dnsType = ec2.DnsRecordIpTypeServiceDefined
	}
	if tables == nil {
		tables = []string{}
	}
	return &ec2.VpcEndpoint{
		CreationTimestamp:   aws.Time(rec.CreatedAt),
		DnsEntries:          []*ec2.DnsEntry{},
		DnsOptions:          &ec2.DnsOptions{DnsRecordIpType: aws.String(dnsType)},
		Groups:              []*ec2.SecurityGroupIdentifier{},
		IpAddressType:       aws.String(ec2.IpAddressTypeIpv4),
		NetworkInterfaceIds: []*string{},
		OwnerId:             aws.String(rec.AccountID),
		PolicyDocument:      aws.String(policy),
		PrivateDnsEnabled:   aws.Bool(false),
		RequesterManaged:    aws.Bool(false),
		RouteTableIds:       aws.StringSlice(tables),
		ServiceName:         aws.String(rec.ServiceName),
		State:               aws.String(StateAvailable),
		SubnetIds:           []*string{},
		Tags:                tagsToEC2(rec.Tags),
		VpcEndpointId:       aws.String(rec.ID),
		VpcEndpointType:     aws.String(rec.Type),
		VpcId:               aws.String(rec.VpcID),
	}
}

func tagsToEC2(tags map[string]string) []*ec2.Tag {
	out := make([]*ec2.Tag, 0, len(tags))
	for _, k := range slices.Sorted(maps.Keys(tags)) {
		out = append(out, &ec2.Tag{Key: aws.String(k), Value: aws.String(tags[k])})
	}
	return out
}

// ServiceID is the stable vpce-svc- ID of a Region's S3 gateway service.
func ServiceID(region string) string {
	sum := sha256.Sum256([]byte(region + "|" + TypeGateway + "|s3"))
	return "vpce-svc-" + hex.EncodeToString(sum[:])[:17]
}

// DescribeVpcEndpointServices lists the S3 gateway service once it is
// offered on this formation.
// MaxResults has no range in the AWS model; a value above 1000 returns 1000.
func (s *ServiceImpl) DescribeVpcEndpointServices(ctx context.Context, input *ec2.DescribeVpcEndpointServicesInput, _ string) (*ec2.DescribeVpcEndpointServicesOutput, error) {
	// One service fits any page, so a valid token needs no resume.
	if _, err := paging.DecodeToken(aws.StringValue(input.NextToken)); err != nil {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"Value ( %s ) for parameter NextToken is invalid. The token is invalid.", aws.StringValue(input.NextToken))
	}
	filters, err := parseFilters(input.Filters, servicesValidFilters)
	if err != nil {
		return nil, err
	}
	active := s.serviceActive(ctx)
	name := s.serviceName()
	for _, n := range aws.StringValueSlice(input.ServiceNames) {
		if !active || n != name {
			return nil, unknownService(n)
		}
	}
	out := &ec2.DescribeVpcEndpointServicesOutput{ServiceDetails: []*ec2.ServiceDetail{}, ServiceNames: []*string{}}
	if !active {
		return out, nil
	}
	values := map[string]string{
		"owner":          "amazon",
		"service-name":   name,
		"service-region": s.settings.Region,
		"service-type":   TypeGateway,
	}
	for f, want := range filters {
		if v, ok := values[f]; ok && !awsfilters.MatchesAny(want, v) {
			return out, nil
		}
	}
	// A tag filter matches nothing: the service carries no tags.
	if !awsfilters.MatchesTags(filters, nil) {
		return out, nil
	}
	out.ServiceDetails = append(out.ServiceDetails, s.serviceDetail())
	out.ServiceNames = append(out.ServiceNames, aws.String(name))
	return out, nil
}

func (s *ServiceImpl) serviceDetail() *ec2.ServiceDetail {
	azs := []*string{}
	if s.settings.AZ != "" {
		azs = append(azs, aws.String(s.settings.AZ))
	}
	return &ec2.ServiceDetail{
		AcceptanceRequired:         aws.Bool(false),
		AvailabilityZones:          azs,
		BaseEndpointDnsNames:       aws.StringSlice([]string{"s3." + s.settings.Region + "." + s.settings.ServicesDomain}),
		ManagesVpcEndpoints:        aws.Bool(false),
		Owner:                      aws.String("amazon"),
		ServiceId:                  aws.String(ServiceID(s.settings.Region)),
		ServiceName:                aws.String(s.serviceName()),
		ServiceType:                []*ec2.ServiceTypeDetail{{ServiceType: aws.String(TypeGateway)}},
		SupportedIpAddressTypes:    aws.StringSlice([]string{ec2.IpAddressTypeIpv4}),
		Tags:                       []*ec2.Tag{},
		VpcEndpointPolicySupported: aws.Bool(true),
	}
}
