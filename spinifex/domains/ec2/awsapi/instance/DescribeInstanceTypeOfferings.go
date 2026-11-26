package instance

import (
	"context"
	"errors"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	ec2zoneapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/zone"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsfilters "github.com/mulgadc/spinifex/spinifex/foundation/aws/filters"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/paging"
	"github.com/nats-io/nats.go"
)

// defaultPageSize caps a paged request that names no MaxResults.
const defaultPageSize = 1000

var describeInstanceTypeOfferingsValidFilters = map[string]bool{
	"instance-type": true,
	"location":      true,
}

// DescribeInstanceTypeOfferings projects the supported instance type catalogue
// over the location list. Both inputs are read live on every call, so the answer
// cannot advertise a type the platform is unable to place.
func DescribeInstanceTypeOfferings(ctx context.Context, input *ec2.DescribeInstanceTypeOfferingsInput, natsConn *nats.Conn, expectedNodes int, accountID, region, az string, ids ec2zoneapi.IDSource) (*ec2.DescribeInstanceTypeOfferingsOutput, error) {
	if input == nil {
		input = &ec2.DescribeInstanceTypeOfferingsInput{}
	}

	locationType := aws.StringValue(input.LocationType)
	if locationType == "" {
		locationType = ec2.LocationTypeRegion
	}

	locations, err := offeringLocations(ctx, locationType, region, az, ids)
	if err != nil {
		slog.WarnContext(ctx, "DescribeInstanceTypeOfferings: invalid location type", "location_type", locationType)
		return nil, err
	}

	parsedFilters, err := awsfilters.ParseFilters(input.Filters, describeInstanceTypeOfferingsValidFilters)
	if err != nil {
		slog.WarnContext(ctx, "DescribeInstanceTypeOfferings: invalid filter", "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	// No capacity filter: an offering is what the platform supports, not what
	// has a free slot at this instant.
	catalogue, err := DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{}, natsConn, expectedNodes, nil, accountID)
	if err != nil {
		return nil, err
	}

	offerings := make([]*ec2.InstanceTypeOffering, 0, len(catalogue.InstanceTypes)*len(locations))
	for _, instanceType := range catalogue.InstanceTypes {
		if instanceType == nil || instanceType.InstanceType == nil {
			continue
		}
		if !awsfilters.MatchesAny(parsedFilters["instance-type"], *instanceType.InstanceType) {
			continue
		}
		for _, location := range locations {
			if !awsfilters.MatchesAny(parsedFilters["location"], location) {
				continue
			}
			offerings = append(offerings, &ec2.InstanceTypeOffering{
				InstanceType: aws.String(*instanceType.InstanceType),
				Location:     aws.String(location),
				LocationType: aws.String(locationType),
			})
		}
	}

	// A space sorts below every character of a type name, so the key orders by
	// type, then location.
	page, nextToken, err := pageByKey(offerings, func(o *ec2.InstanceTypeOffering) string {
		return *o.InstanceType + " " + *o.Location
	}, input.NextToken, input.MaxResults)
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
func offeringLocations(ctx context.Context, locationType, region, az string, ids ec2zoneapi.IDSource) ([]string, error) {
	switch locationType {
	case ec2.LocationTypeRegion:
		return []string{region}, nil
	case ec2.LocationTypeAvailabilityZone, ec2.LocationTypeAvailabilityZoneId:
	default:
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	zones, err := ec2zoneapi.DescribeAvailabilityZones(ctx, &ec2.DescribeAvailabilityZonesInput{}, region, az, ids)
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

// pageByKey pages items by key with an opaque resume token. A token this
// service did not issue is refused. The last page carries a nil token.
func pageByKey[T any](items []T, key func(T) string, token *string, maxResults *int64) ([]T, *string, error) {
	from, err := paging.DecodeToken(aws.StringValue(token))
	if err != nil {
		return nil, nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"Value ( %s ) for parameter NextToken is invalid. The token is invalid.", aws.StringValue(token))
	}
	size := defaultPageSize
	if maxResults != nil && *maxResults > 0 {
		size = int(*maxResults)
	}
	page, next := paging.EC2Page(items, key, paging.Request{Limit: size, From: from})
	return page, next, nil
}
