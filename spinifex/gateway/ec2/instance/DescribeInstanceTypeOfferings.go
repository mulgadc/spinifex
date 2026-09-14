package gateway_ec2_instance

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strconv"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/filterutil"
	gateway_ec2_zone "github.com/mulgadc/spinifex/spinifex/gateway/ec2/zone"
	"github.com/nats-io/nats.go"
)

// defaultOfferingsPerPage caps a request that names no MaxResults.
const defaultOfferingsPerPage = 1000

var describeInstanceTypeOfferingsValidFilters = map[string]bool{
	"instance-type": true,
	"location":      true,
}

// DescribeInstanceTypeOfferings projects the supported instance type catalogue
// over the location list. Both inputs are read live on every call, so the answer
// cannot advertise a type the platform is unable to place.
func DescribeInstanceTypeOfferings(ctx context.Context, input *ec2.DescribeInstanceTypeOfferingsInput, natsConn *nats.Conn, expectedNodes int, accountID, region, az string) (*ec2.DescribeInstanceTypeOfferingsOutput, error) {
	if input == nil {
		input = &ec2.DescribeInstanceTypeOfferingsInput{}
	}

	locationType := aws.StringValue(input.LocationType)
	if locationType == "" {
		locationType = ec2.LocationTypeRegion
	}

	locations, err := offeringLocations(locationType, region, az)
	if err != nil {
		slog.WarnContext(ctx, "DescribeInstanceTypeOfferings: invalid location type", "location_type", locationType)
		return nil, err
	}

	parsedFilters, err := filterutil.ParseFilters(input.Filters, describeInstanceTypeOfferingsValidFilters)
	if err != nil {
		slog.WarnContext(ctx, "DescribeInstanceTypeOfferings: invalid filter", "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	// No capacity filter: an offering is what the platform supports, not what
	// has a free slot at this instant.
	catalogue, err := DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{}, natsConn, expectedNodes, accountID)
	if err != nil {
		return nil, err
	}

	offerings := make([]*ec2.InstanceTypeOffering, 0, len(catalogue.InstanceTypes)*len(locations))
	for _, instanceType := range catalogue.InstanceTypes {
		if instanceType == nil || instanceType.InstanceType == nil {
			continue
		}
		if !filterutil.MatchesAny(parsedFilters["instance-type"], *instanceType.InstanceType) {
			continue
		}
		for _, location := range locations {
			if !filterutil.MatchesAny(parsedFilters["location"], location) {
				continue
			}
			offerings = append(offerings, &ec2.InstanceTypeOffering{
				InstanceType: aws.String(*instanceType.InstanceType),
				Location:     aws.String(location),
				LocationType: aws.String(locationType),
			})
		}
	}

	// Total order so a paged traversal sees every offering exactly once.
	sort.Slice(offerings, func(i, j int) bool {
		if *offerings[i].InstanceType != *offerings[j].InstanceType {
			return *offerings[i].InstanceType < *offerings[j].InstanceType
		}
		return *offerings[i].Location < *offerings[j].Location
	})

	page, nextToken, err := pageOfferings(offerings, input.NextToken, input.MaxResults)
	if err != nil {
		slog.WarnContext(ctx, "DescribeInstanceTypeOfferings: invalid NextToken", "next_token", aws.StringValue(input.NextToken))
		return nil, err
	}

	slog.InfoContext(ctx, "DescribeInstanceTypeOfferings: derived offerings", "location_type", locationType, "total", len(offerings), "returned", len(page))
	return &ec2.DescribeInstanceTypeOfferingsOutput{InstanceTypeOfferings: page, NextToken: nextToken}, nil
}

// offeringLocations resolves the locations an offering can be reported at. The
// zone list is read rather than assumed, so the operation follows the zone model
// if it ever widens.
func offeringLocations(locationType, region, az string) ([]string, error) {
	switch locationType {
	case ec2.LocationTypeRegion:
		return []string{region}, nil
	case ec2.LocationTypeAvailabilityZone, ec2.LocationTypeAvailabilityZoneId:
	default:
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	zones, err := gateway_ec2_zone.DescribeAvailabilityZones(&ec2.DescribeAvailabilityZonesInput{}, region, az)
	if err != nil {
		return nil, err
	}

	locations := make([]string, 0, len(zones.AvailabilityZones))
	for _, zone := range zones.AvailabilityZones {
		if locationType == ec2.LocationTypeAvailabilityZoneId {
			locations = append(locations, aws.StringValue(zone.ZoneId))
			continue
		}
		locations = append(locations, aws.StringValue(zone.ZoneName))
	}
	return locations, nil
}

// pageOfferings slices one page out of a sorted offering set, using an opaque
// integer offset as the token. The last page carries a nil token.
func pageOfferings(offerings []*ec2.InstanceTypeOffering, token *string, maxResults *int64) ([]*ec2.InstanceTypeOffering, *string, error) {
	start := 0
	if aws.StringValue(token) != "" {
		n, err := strconv.Atoi(*token)
		if err != nil || n < 0 {
			return nil, nil, errors.New(awserrors.ErrorInvalidParameterValue)
		}
		start = n
	}
	if start > len(offerings) {
		start = len(offerings)
	}

	size := defaultOfferingsPerPage
	if maxResults != nil && *maxResults > 0 {
		size = int(*maxResults)
	}

	end := start + size
	if end >= len(offerings) {
		return offerings[start:], nil, nil
	}
	return offerings[start:end], aws.String(strconv.Itoa(end)), nil
}
