package gateway_ec2_zone

import (
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	filterutil "github.com/mulgadc/spinifex/spinifex/foundation/aws/filters"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
)

// The filter names AWS accepts on each call; any other, tag filters included,
// is refused.
var (
	describeAvailabilityZonesValidFilters = map[string]bool{
		"group-long-name":  true,
		"group-name":       true,
		"message":          true,
		"opt-in-status":    true,
		"parent-zone-id":   true,
		"parent-zone-name": true,
		"region-name":      true,
		"state":            true,
		"zone-id":          true,
		"zone-name":        true,
		"zone-type":        true,
	}
	describeRegionsValidFilters = map[string]bool{
		"endpoint":      true,
		"opt-in-status": true,
		"region-name":   true,
	}
)

func DescribeAvailabilityZones(input *ec2.DescribeAvailabilityZonesInput, region string, az string) (output *ec2.DescribeAvailabilityZonesOutput, err error) {
	filters, err := parseFilters(input.Filters, describeAvailabilityZonesValidFilters)
	if err != nil {
		return nil, err
	}

	zone := &ec2.AvailabilityZone{
		State:              aws.String("available"),
		OptInStatus:        aws.String("opt-in-not-required"),
		RegionName:         aws.String(region),
		ZoneName:           aws.String(az),
		ZoneId:             aws.String(handlers_ec2_vpc.SingleZoneID),
		GroupName:          aws.String(region),
		NetworkBorderGroup: aws.String(region),
		ZoneType:           aws.String("availability-zone"),
		Messages:           []*ec2.AvailabilityZoneMessage{},
	}

	// AWS names the first unknown zone in the order the caller gave them.
	for _, name := range aws.StringValueSlice(input.ZoneNames) {
		if name != az {
			return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "Invalid availability zone: [%s]", name)
		}
	}
	for _, id := range aws.StringValueSlice(input.ZoneIds) {
		if id != handlers_ec2_vpc.SingleZoneID {
			return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "Invalid availability zone-id: [%s]", id)
		}
	}

	output = &ec2.DescribeAvailabilityZonesOutput{AvailabilityZones: []*ec2.AvailabilityZone{}}
	if matchesFilters(filters, zoneFilterValues(zone)) {
		output.AvailabilityZones = append(output.AvailabilityZones, zone)
	}
	return output, nil
}

// DescribeRegions reports the current Region and a dialable endpoint. The
// caller resolves endpoint from the gateway's advertised host so a remote
// workload is never pointed at its own loopback.
func DescribeRegions(input *ec2.DescribeRegionsInput, region string, endpoint string) (output *ec2.DescribeRegionsOutput, err error) {
	filters, err := parseFilters(input.Filters, describeRegionsValidFilters)
	if err != nil {
		return nil, err
	}

	// AWS names the alphabetically first unknown Region, not the first given.
	var unknown []string
	for _, name := range aws.StringValueSlice(input.RegionNames) {
		if name != region {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "Invalid region: [%s]", slices.Min(unknown))
	}

	r := &ec2.Region{
		Endpoint:    aws.String(endpoint),
		RegionName:  aws.String(region),
		OptInStatus: aws.String("opt-in-not-required"),
	}
	output = &ec2.DescribeRegionsOutput{Regions: []*ec2.Region{}}
	if matchesFilters(filters, map[string][]string{
		"endpoint":      {endpoint},
		"opt-in-status": {aws.StringValue(r.OptInStatus)},
		"region-name":   {region},
	}) {
		output.Regions = append(output.Regions, r)
	}
	return output, nil
}

// parseFilters is filterutil.ParseFilters without its blanket acceptance of
// tag filters, which AWS refuses on both calls.
func parseFilters(filters []*ec2.Filter, valid map[string]bool) (map[string][]string, error) {
	for _, f := range filters {
		if name := aws.StringValue(f.Name); strings.HasPrefix(name, "tag:") {
			return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "The filter '%s' is invalid", name)
		}
	}
	return filterutil.ParseFilters(filters, valid)
}

// zoneFilterValues maps each filter name to the zone's values for it. A name
// with no values, such as parent-zone-id on a zone with no parent, matches nothing.
func zoneFilterValues(zone *ec2.AvailabilityZone) map[string][]string {
	values := map[string][]string{
		"group-name":    {aws.StringValue(zone.GroupName)},
		"opt-in-status": {aws.StringValue(zone.OptInStatus)},
		"region-name":   {aws.StringValue(zone.RegionName)},
		"state":         {aws.StringValue(zone.State)},
		"zone-id":       {aws.StringValue(zone.ZoneId)},
		"zone-name":     {aws.StringValue(zone.ZoneName)},
		"zone-type":     {aws.StringValue(zone.ZoneType)},
	}
	for _, m := range zone.Messages {
		values["message"] = append(values["message"], aws.StringValue(m.Message))
	}
	return values
}

// matchesFilters reports whether every filter matches one of the resource's
// values for that name.
func matchesFilters(filters map[string][]string, values map[string][]string) bool {
	for name, patterns := range filters {
		if !slices.ContainsFunc(values[name], func(v string) bool { return filterutil.MatchesAny(patterns, v) }) {
			return false
		}
	}
	return true
}
