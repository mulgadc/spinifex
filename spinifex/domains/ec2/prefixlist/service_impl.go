package prefixlist

import (
	"context"
	"slices"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsfilters "github.com/mulgadc/spinifex/spinifex/foundation/aws/filters"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/paging"
)

// ServiceImpl answers the prefix-list reads from the node set; each costs the
// node count.
type ServiceImpl struct {
	region  string
	cluster func() *config.ClusterConfig
}

// NewServiceImpl reads the node set through cluster on every call.
func NewServiceImpl(region string, cluster func() *config.ClusterConfig) *ServiceImpl {
	return &ServiceImpl{region: region, cluster: cluster}
}

func (s *ServiceImpl) cidrs() []string {
	entries := Entries(s.cluster())
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.CIDR())
	}
	return out
}

func (s *ServiceImpl) notFound(id string) error {
	return awserrors.Errorf(awserrors.ErrorInvalidPrefixListIDNotFound, "The prefix list (%s) does not exist.", id)
}

// selected applies the ID list and the name, ID and owner filters to the one list.
func (s *ServiceImpl) selected(ids []*string, filters map[string][]string) (bool, error) {
	id := ID(s.region)
	for _, want := range awsidentifiers.Distinct(ids) {
		if want != id {
			return false, s.notFound(want)
		}
	}
	for name, values := range filters {
		var value string
		switch name {
		case "prefix-list-id":
			value = id
		case "prefix-list-name":
			value = ServiceName(s.region)
		case "owner-id":
			value = OwnerID
		}
		if !awsfilters.MatchesAny(values, value) {
			return false, nil
		}
	}
	return true, nil
}

var describePaging = paging.EC2{
	MaxResults: 1000,
	TooLarge:   "Value ( %d ) for parameter maxResults is invalid. Expecting a value less than or equal to 1000.",
	TooSmall:   "Value ( %d ) for parameter maxResults is invalid. Expecting a value greater than or equal to 5.",
	WithIDs:    "The parameter prefixListIds cannot be used with the parameter maxResults",
}

// DescribePrefixLists lists the S3 list with its CIDRs.
func (s *ServiceImpl) DescribePrefixLists(_ context.Context, input *ec2.DescribePrefixListsInput, _ string) (*ec2.DescribePrefixListsOutput, error) {
	if _, err := describePaging.Parse(input.MaxResults, input.NextToken, len(input.PrefixListIds)); err != nil {
		return nil, err
	}
	filters, err := awsfilters.ParseFilters(input.Filters, map[string]bool{"prefix-list-id": true, "prefix-list-name": true})
	if err != nil {
		return nil, err
	}
	ok, err := s.selected(input.PrefixListIds, filters)
	if err != nil {
		return nil, err
	}
	out := &ec2.DescribePrefixListsOutput{PrefixLists: []*ec2.PrefixList{}}
	if ok {
		out.PrefixLists = append(out.PrefixLists, &ec2.PrefixList{
			PrefixListId:   aws.String(ID(s.region)),
			PrefixListName: aws.String(ServiceName(s.region)),
			Cidrs:          aws.StringSlice(s.cidrs()),
		})
	}
	return out, nil
}

// smallPage validates the 1 to 100 MaxResults of the managed-list reads.
func smallPage(maxResults *int64, nextToken *string) (paging.Request, error) {
	if maxResults != nil && (*maxResults < 1 || *maxResults > 100) {
		return paging.Request{}, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"Value ( %d ) for parameter maxResults is invalid. Expecting a value between 1 and 100.", *maxResults)
	}
	from, err := paging.DecodeToken(aws.StringValue(nextToken))
	if err != nil {
		return paging.Request{}, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"Value ( %s ) for parameter NextToken is invalid. The token is invalid.", aws.StringValue(nextToken))
	}
	return paging.Request{Limit: int(aws.Int64Value(maxResults)), From: from}, nil
}

// DescribeManagedPrefixLists lists the S3 list as AWS renders an AWS-managed
// list: owned by AWS, with no version and no entry limit.
func (s *ServiceImpl) DescribeManagedPrefixLists(_ context.Context, input *ec2.DescribeManagedPrefixListsInput, _ string) (*ec2.DescribeManagedPrefixListsOutput, error) {
	if _, err := smallPage(input.MaxResults, input.NextToken); err != nil {
		return nil, err
	}
	filters, err := awsfilters.ParseFilters(input.Filters, map[string]bool{"prefix-list-id": true, "prefix-list-name": true, "owner-id": true})
	if err != nil {
		return nil, err
	}
	ok, err := s.selected(input.PrefixListIds, filters)
	if err != nil {
		return nil, err
	}
	out := &ec2.DescribeManagedPrefixListsOutput{PrefixLists: []*ec2.ManagedPrefixList{}}
	if ok {
		out.PrefixLists = append(out.PrefixLists, &ec2.ManagedPrefixList{
			PrefixListId:   aws.String(ID(s.region)),
			PrefixListName: aws.String(ServiceName(s.region)),
			PrefixListArn:  aws.String(ARN(s.region)),
			OwnerId:        aws.String(OwnerID),
			State:          aws.String(ec2.PrefixListStateCreateComplete),
			AddressFamily:  aws.String("IPv4"),
			Tags:           []*ec2.Tag{},
		})
	}
	return out, nil
}

// GetManagedPrefixListEntries pages the list's CIDRs. AWS shows no version for
// an AWS-managed list, so any TargetVersion is refused.
func (s *ServiceImpl) GetManagedPrefixListEntries(_ context.Context, input *ec2.GetManagedPrefixListEntriesInput, _ string) (*ec2.GetManagedPrefixListEntriesOutput, error) {
	id := aws.StringValue(input.PrefixListId)
	if id == "" {
		return nil, awserrors.Errorf(awserrors.ErrorMissingParameter, "The request must contain the parameter PrefixListId")
	}
	if id != ID(s.region) {
		return nil, s.notFound(id)
	}
	if input.TargetVersion != nil {
		return nil, awserrors.Errorf(awserrors.ErrorUnsupportedOperation, "Version is not supported for an AWS-managed prefix list.")
	}
	req, err := smallPage(input.MaxResults, input.NextToken)
	if err != nil {
		return nil, err
	}
	entries := make([]*ec2.PrefixListEntry, 0)
	for _, c := range s.cidrs() {
		entries = append(entries, &ec2.PrefixListEntry{Cidr: aws.String(c)})
	}
	page, next := paging.EC2Page(entries, func(e *ec2.PrefixListEntry) string { return *e.Cidr }, req)
	if page == nil {
		page = []*ec2.PrefixListEntry{}
	}
	return &ec2.GetManagedPrefixListEntriesOutput{Entries: slices.Clip(page), NextToken: next}, nil
}
