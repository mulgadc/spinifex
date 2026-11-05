package zone

import (
	"testing"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/zoneid"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newIDs(t *testing.T, region, override string) *zoneid.Resolver {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	r, err := zoneid.NewResolver(js, region, override)
	require.NoError(t, err)
	return r
}

func TestDescribeAvailabilityZones(t *testing.T) {
	tests := []struct {
		name   string
		region string
		az     string
		zoneID string
	}{
		{
			name:   "Sydney",
			region: "ap-southeast-2",
			az:     "ap-southeast-2a",
			zoneID: "apse2-az1",
		},
		{
			name:   "US East",
			region: "us-east-1",
			az:     "us-east-1a",
			zoneID: "use1-az1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := &ec2.DescribeAvailabilityZonesInput{}
			output, err := DescribeAvailabilityZones(t.Context(), input, tt.region, tt.az, newIDs(t, tt.region, ""))

			require.NoError(t, err)
			require.NotNil(t, output)
			require.Len(t, output.AvailabilityZones, 1)

			zone := output.AvailabilityZones[0]
			assert.Equal(t, "available", *zone.State)
			assert.Equal(t, "opt-in-not-required", *zone.OptInStatus)
			assert.Equal(t, tt.region, *zone.RegionName)
			assert.Equal(t, tt.az, *zone.ZoneName)
			assert.Equal(t, tt.zoneID, *zone.ZoneId)
			assert.Equal(t, tt.region, *zone.GroupName)
			assert.Equal(t, tt.region, *zone.NetworkBorderGroup)
			assert.Equal(t, "availability-zone", *zone.ZoneType)
			assert.Empty(t, zone.Messages)
		})
	}
}

func TestDescribeAvailabilityZones_ZoneIDsSelectBySydneyID(t *testing.T) {
	ids := newIDs(t, "ap-southeast-2", "")
	out, err := DescribeAvailabilityZones(t.Context(), &ec2.DescribeAvailabilityZonesInput{ZoneIds: aws.StringSlice([]string{"apse2-az1"})}, "ap-southeast-2", "ap-southeast-2a", ids)
	require.NoError(t, err)
	require.Len(t, out.AvailabilityZones, 1)
	assert.Equal(t, "ap-southeast-2a", aws.StringValue(out.AvailabilityZones[0].ZoneName))
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
	ids := newIDs(t, region, "")

	for _, tc := range []struct {
		name  string
		input *ec2.DescribeAvailabilityZonesInput
		want  int
	}{
		{"zone name", &ec2.DescribeAvailabilityZonesInput{ZoneNames: aws.StringSlice([]string{az})}, 1},
		{"zone id", &ec2.DescribeAvailabilityZonesInput{ZoneIds: aws.StringSlice([]string{"muw1-az1"})}, 1},
		{"zone-name filter", &ec2.DescribeAvailabilityZonesInput{Filters: filter("zone-name", az)}, 1},
		{"wildcard filter", &ec2.DescribeAvailabilityZonesInput{Filters: filter("region-name", "mulga-*")}, 1},
		{"state filter", &ec2.DescribeAvailabilityZonesInput{Filters: filter("state", "available")}, 1},
		{"filter matching nothing", &ec2.DescribeAvailabilityZonesInput{Filters: filter("zone-name", "other")}, 0},
		{"filter on an absent field", &ec2.DescribeAvailabilityZonesInput{Filters: filter("parent-zone-id", "*")}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := DescribeAvailabilityZones(t.Context(), tc.input, region, az, ids)
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
		{"unknown zone id", &ec2.DescribeAvailabilityZonesInput{ZoneIds: aws.StringSlice([]string{"muw1-az1", "zz-az9"})}, "Invalid availability zone-id: [zz-az9]"},
		{"the retired constant is an unknown zone id", &ec2.DescribeAvailabilityZonesInput{ZoneIds: aws.StringSlice([]string{"spinifexz1"})}, "Invalid availability zone-id: [spinifexz1]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DescribeAvailabilityZones(t.Context(), tc.input, region, az, ids)
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

func TestDescribeAvailabilityZones_NoIDSourceFailsClosed(t *testing.T) {
	_, err := DescribeAvailabilityZones(t.Context(), &ec2.DescribeAvailabilityZonesInput{}, "mulga-lab", "mulga-laba", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorServerInternal)
}
