package gateway_ec2_zone

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDescribeAvailabilityZones(t *testing.T) {
	tests := []struct {
		name   string
		region string
		az     string
	}{
		{
			name:   "Sydney",
			region: "ap-southeast-2",
			az:     "ap-southeast-2a",
		},
		{
			name:   "US East",
			region: "us-east-1",
			az:     "us-east-1a",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := &ec2.DescribeAvailabilityZonesInput{}
			output, err := DescribeAvailabilityZones(input, tt.region, tt.az)

			require.NoError(t, err)
			require.NotNil(t, output)
			require.Len(t, output.AvailabilityZones, 1)

			zone := output.AvailabilityZones[0]
			assert.Equal(t, "available", *zone.State)
			assert.Equal(t, "opt-in-not-required", *zone.OptInStatus)
			assert.Equal(t, tt.region, *zone.RegionName)
			assert.Equal(t, tt.az, *zone.ZoneName)
			assert.Equal(t, "spinifexz1", *zone.ZoneId)
			assert.Equal(t, tt.region, *zone.GroupName)
			assert.Equal(t, tt.region, *zone.NetworkBorderGroup)
			assert.Equal(t, "availability-zone", *zone.ZoneType)
			assert.Empty(t, zone.Messages)
		})
	}
}

func TestDescribeRegions(t *testing.T) {
	tests := []struct {
		name     string
		region   string
		endpoint string
	}{
		{
			name:     "Sydney",
			region:   "ap-southeast-2",
			endpoint: "https://10.0.0.5:9999",
		},
		{
			name:     "US East",
			region:   "us-east-1",
			endpoint: "https://gw.example.com:9999",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := &ec2.DescribeRegionsInput{}
			output, err := DescribeRegions(input, tt.region, tt.endpoint)

			require.NoError(t, err)
			require.NotNil(t, output)
			require.Len(t, output.Regions, 1)

			region := output.Regions[0]
			// The endpoint is resolved by the caller and must be echoed back
			// verbatim, never a literal this function invents itself.
			assert.Equal(t, tt.endpoint, *region.Endpoint)
			assert.Equal(t, tt.region, *region.RegionName)
			assert.Equal(t, "opt-in-not-required", *region.OptInStatus)
		})
	}
}

func filter(name string, values ...string) []*ec2.Filter {
	return []*ec2.Filter{{Name: aws.String(name), Values: aws.StringSlice(values)}}
}

func requireInvalidParameterValue(t *testing.T, err error, msg string) {
	t.Helper()
	code, got, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, code)
	assert.Equal(t, msg, got)
}

// Names are checked against the configured catalogue, so a custom Region name
// is as valid as an AWS one. The messages are AWS's own.
func TestDescribeAvailabilityZones_Selection(t *testing.T) {
	const region, az = "mulga-west", "mulga-west-a"

	for _, tc := range []struct {
		name  string
		input *ec2.DescribeAvailabilityZonesInput
		want  int
	}{
		{"zone name", &ec2.DescribeAvailabilityZonesInput{ZoneNames: aws.StringSlice([]string{az})}, 1},
		{"zone id", &ec2.DescribeAvailabilityZonesInput{ZoneIds: aws.StringSlice([]string{"spinifexz1"})}, 1},
		{"zone-name filter", &ec2.DescribeAvailabilityZonesInput{Filters: filter("zone-name", az)}, 1},
		{"wildcard filter", &ec2.DescribeAvailabilityZonesInput{Filters: filter("region-name", "mulga-*")}, 1},
		{"state filter", &ec2.DescribeAvailabilityZonesInput{Filters: filter("state", "available")}, 1},
		{"filter matching nothing", &ec2.DescribeAvailabilityZonesInput{Filters: filter("zone-name", "other")}, 0},
		{"filter on an absent field", &ec2.DescribeAvailabilityZonesInput{Filters: filter("parent-zone-id", "*")}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := DescribeAvailabilityZones(tc.input, region, az)
			require.NoError(t, err)
			assert.Len(t, out.AvailabilityZones, tc.want)
		})
	}

	for _, tc := range []struct {
		name  string
		input *ec2.DescribeAvailabilityZonesInput
		msg   string
	}{
		{"unknown filter", &ec2.DescribeAvailabilityZonesInput{Filters: filter("bogus-filter", "x")}, "The filter 'bogus-filter' is invalid"},
		{"tag filter", &ec2.DescribeAvailabilityZonesInput{Filters: filter("tag:Name", "x")}, "The filter 'tag:Name' is invalid"},
		{"network-border-group filter", &ec2.DescribeAvailabilityZonesInput{Filters: filter("network-border-group", "x")}, "The filter 'network-border-group' is invalid"},
		{"first unknown name in request order", &ec2.DescribeAvailabilityZonesInput{ZoneNames: aws.StringSlice([]string{az, "zz-nowhere-9z", "yy-nowhere-8y"})}, "Invalid availability zone: [zz-nowhere-9z]"},
		{"unknown zone id", &ec2.DescribeAvailabilityZonesInput{ZoneIds: aws.StringSlice([]string{"spinifexz1", "zz-az9"})}, "Invalid availability zone-id: [zz-az9]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DescribeAvailabilityZones(tc.input, region, az)
			requireInvalidParameterValue(t, err, tc.msg)
		})
	}
}

func TestDescribeRegions_Selection(t *testing.T) {
	const region, endpoint = "mulga-west", "https://10.0.0.1:9999"

	for _, tc := range []struct {
		name  string
		input *ec2.DescribeRegionsInput
		want  int
	}{
		{"region name", &ec2.DescribeRegionsInput{RegionNames: aws.StringSlice([]string{region})}, 1},
		{"region-name filter", &ec2.DescribeRegionsInput{Filters: filter("region-name", region)}, 1},
		{"endpoint filter", &ec2.DescribeRegionsInput{Filters: filter("endpoint", "*10.0.0.1*")}, 1},
		{"filter matching nothing", &ec2.DescribeRegionsInput{Filters: filter("region-name", "zz-nowhere-9")}, 0},
		{"names and a disagreeing filter", &ec2.DescribeRegionsInput{RegionNames: aws.StringSlice([]string{region}), Filters: filter("region-name", "us-east-1")}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := DescribeRegions(tc.input, region, endpoint)
			require.NoError(t, err)
			assert.Len(t, out.Regions, tc.want)
		})
	}

	for _, tc := range []struct {
		name  string
		input *ec2.DescribeRegionsInput
		msg   string
	}{
		{"unknown filter", &ec2.DescribeRegionsInput{Filters: filter("bogus-filter", "x")}, "The filter 'bogus-filter' is invalid"},
		{"tag filter", &ec2.DescribeRegionsInput{Filters: filter("tag:Name", "x")}, "The filter 'tag:Name' is invalid"},
		{"an AWS region this cluster is not", &ec2.DescribeRegionsInput{RegionNames: aws.StringSlice([]string{"us-east-1"})}, "Invalid region: [us-east-1]"},
		{"alphabetically first unknown name", &ec2.DescribeRegionsInput{RegionNames: aws.StringSlice([]string{region, "zz-nowhere-9", "yy-nowhere-8"})}, "Invalid region: [yy-nowhere-8]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DescribeRegions(tc.input, region, endpoint)
			requireInvalidParameterValue(t, err, tc.msg)
		})
	}
}
