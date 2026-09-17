package gateway_ec2_zone

import (
	"testing"

	"github.com/aws/aws-sdk-go/service/ec2"
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
