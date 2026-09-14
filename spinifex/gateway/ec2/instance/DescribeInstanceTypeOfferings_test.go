package gateway_ec2_instance

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_ec2_zone "github.com/mulgadc/spinifex/spinifex/gateway/ec2/zone"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	offeringsTestRegion = "ap-southeast-2"
	offeringsTestAZ     = "ap-southeast-2a"
)

// serveInstanceTypes stands in for the node catalogue behind DescribeInstanceTypes.
func serveInstanceTypes(t *testing.T, nc *nats.Conn, instanceTypes ...string) {
	t.Helper()

	infos := make([]*ec2.InstanceTypeInfo, 0, len(instanceTypes))
	for _, it := range instanceTypes {
		infos = append(infos, &ec2.InstanceTypeInfo{InstanceType: aws.String(it)})
	}

	_, err := nc.Subscribe("ec2.DescribeInstanceTypes", func(msg *nats.Msg) {
		data, _ := json.Marshal(&ec2.DescribeInstanceTypesOutput{InstanceTypes: infos})
		msg.Respond(data)
	})
	require.NoError(t, err)
	require.NoError(t, nc.Flush())
}

// TestDescribeInstanceTypeOfferings_AvailabilityZone asserts the result against
// DescribeInstanceTypes and DescribeAvailabilityZones rather than a fixture, so
// the test fails if the answer ever stops being derived from them.
func TestDescribeInstanceTypeOfferings_AvailabilityZone(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	serveInstanceTypes(t, nc, "t3.micro", "m5.large")

	types, err := DescribeInstanceTypes(context.Background(), &ec2.DescribeInstanceTypesInput{}, nc, 1, "")
	require.NoError(t, err)
	zones, err := gateway_ec2_zone.DescribeAvailabilityZones(&ec2.DescribeAvailabilityZonesInput{}, offeringsTestRegion, offeringsTestAZ)
	require.NoError(t, err)

	output, err := DescribeInstanceTypeOfferings(context.Background(), &ec2.DescribeInstanceTypeOfferingsInput{
		LocationType: aws.String(ec2.LocationTypeAvailabilityZone),
	}, nc, 1, "", offeringsTestRegion, offeringsTestAZ)
	require.NoError(t, err)

	var want []*ec2.InstanceTypeOffering
	for _, it := range types.InstanceTypes {
		for _, zone := range zones.AvailabilityZones {
			want = append(want, &ec2.InstanceTypeOffering{
				InstanceType: it.InstanceType,
				Location:     zone.ZoneName,
				LocationType: aws.String(ec2.LocationTypeAvailabilityZone),
			})
		}
	}

	assert.ElementsMatch(t, want, output.InstanceTypeOfferings)
	assert.Nil(t, output.NextToken)
}

func TestDescribeInstanceTypeOfferings_Region(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	serveInstanceTypes(t, nc, "t3.micro", "m5.large")

	output, err := DescribeInstanceTypeOfferings(context.Background(), &ec2.DescribeInstanceTypeOfferingsInput{
		LocationType: aws.String(ec2.LocationTypeRegion),
	}, nc, 1, "", offeringsTestRegion, offeringsTestAZ)
	require.NoError(t, err)

	require.Len(t, output.InstanceTypeOfferings, 2)
	for _, offering := range output.InstanceTypeOfferings {
		assert.Equal(t, offeringsTestRegion, aws.StringValue(offering.Location))
		assert.Equal(t, ec2.LocationTypeRegion, aws.StringValue(offering.LocationType))
	}
}

// TestDescribeInstanceTypeOfferings_DefaultLocationTypeIsRegion covers the AWS
// default: an omitted LocationType reports the region, not a zone.
func TestDescribeInstanceTypeOfferings_DefaultLocationTypeIsRegion(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	serveInstanceTypes(t, nc, "t3.micro")

	output, err := DescribeInstanceTypeOfferings(context.Background(), &ec2.DescribeInstanceTypeOfferingsInput{}, nc, 1, "", offeringsTestRegion, offeringsTestAZ)
	require.NoError(t, err)

	require.Len(t, output.InstanceTypeOfferings, 1)
	assert.Equal(t, offeringsTestRegion, aws.StringValue(output.InstanceTypeOfferings[0].Location))
	assert.Equal(t, ec2.LocationTypeRegion, aws.StringValue(output.InstanceTypeOfferings[0].LocationType))
}

func TestDescribeInstanceTypeOfferings_AvailabilityZoneID(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	serveInstanceTypes(t, nc, "t3.micro")

	zones, err := gateway_ec2_zone.DescribeAvailabilityZones(&ec2.DescribeAvailabilityZonesInput{}, offeringsTestRegion, offeringsTestAZ)
	require.NoError(t, err)
	zoneID := aws.StringValue(zones.AvailabilityZones[0].ZoneId)
	require.NotEqual(t, offeringsTestAZ, zoneID, "zone ID must differ from zone name for this assertion to mean anything")

	output, err := DescribeInstanceTypeOfferings(context.Background(), &ec2.DescribeInstanceTypeOfferingsInput{
		LocationType: aws.String(ec2.LocationTypeAvailabilityZoneId),
	}, nc, 1, "", offeringsTestRegion, offeringsTestAZ)
	require.NoError(t, err)

	require.Len(t, output.InstanceTypeOfferings, 1)
	assert.Equal(t, zoneID, aws.StringValue(output.InstanceTypeOfferings[0].Location))
}

func TestDescribeInstanceTypeOfferings_UnknownLocationType(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	serveInstanceTypes(t, nc, "t3.micro")

	_, err := DescribeInstanceTypeOfferings(context.Background(), &ec2.DescribeInstanceTypeOfferingsInput{
		LocationType: aws.String("outpost"),
	}, nc, 1, "", offeringsTestRegion, offeringsTestAZ)

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}

func TestDescribeInstanceTypeOfferings_Filters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		filters   []*ec2.Filter
		wantTypes []string
	}{
		{
			name:      "instance-type narrows",
			filters:   []*ec2.Filter{{Name: aws.String("instance-type"), Values: []*string{aws.String("t3.micro")}}},
			wantTypes: []string{"t3.micro"},
		},
		{
			name:      "instance-type wildcard narrows",
			filters:   []*ec2.Filter{{Name: aws.String("instance-type"), Values: []*string{aws.String("t3.*")}}},
			wantTypes: []string{"t3.micro", "t3.small"},
		},
		{
			name:      "location narrows to the zone",
			filters:   []*ec2.Filter{{Name: aws.String("location"), Values: []*string{aws.String(offeringsTestAZ)}}},
			wantTypes: []string{"m5.large", "t3.micro", "t3.small"},
		},
		{
			name:      "location matching nothing is empty, not an error",
			filters:   []*ec2.Filter{{Name: aws.String("location"), Values: []*string{aws.String("us-east-1a")}}},
			wantTypes: nil,
		},
		{
			name:      "instance-type matching nothing is empty, not an error",
			filters:   []*ec2.Filter{{Name: aws.String("instance-type"), Values: []*string{aws.String("c9.nonexistent")}}},
			wantTypes: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, nc := startTestNATSServer(t)
			serveInstanceTypes(t, nc, "t3.micro", "t3.small", "m5.large")

			output, err := DescribeInstanceTypeOfferings(context.Background(), &ec2.DescribeInstanceTypeOfferingsInput{
				LocationType: aws.String(ec2.LocationTypeAvailabilityZone),
				Filters:      tc.filters,
			}, nc, 1, "", offeringsTestRegion, offeringsTestAZ)
			require.NoError(t, err)

			var got []string
			for _, offering := range output.InstanceTypeOfferings {
				got = append(got, aws.StringValue(offering.InstanceType))
			}
			assert.Equal(t, tc.wantTypes, got)
			assert.NotNil(t, output.InstanceTypeOfferings, "empty result must render its container, so the slice must not be nil")
		})
	}
}

func TestDescribeInstanceTypeOfferings_UnknownFilter(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	serveInstanceTypes(t, nc, "t3.micro")

	_, err := DescribeInstanceTypeOfferings(context.Background(), &ec2.DescribeInstanceTypeOfferingsInput{
		LocationType: aws.String(ec2.LocationTypeAvailabilityZone),
		Filters:      []*ec2.Filter{{Name: aws.String("not-a-filter"), Values: []*string{aws.String("x")}}},
	}, nc, 1, "", offeringsTestRegion, offeringsTestAZ)

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}

// TestDescribeInstanceTypeOfferings_FullTraversal walks every page with a small
// MaxResults and asserts each offering appears exactly once, so a paging caller
// cannot loop or miss a type.
func TestDescribeInstanceTypeOfferings_FullTraversal(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	catalogue := []string{"c5.large", "m5.large", "m5.xlarge", "t3.micro", "t3.small"}
	serveInstanceTypes(t, nc, catalogue...)

	seen := make(map[string]int)
	var token *string
	pages := 0
	for {
		output, err := DescribeInstanceTypeOfferings(context.Background(), &ec2.DescribeInstanceTypeOfferingsInput{
			LocationType: aws.String(ec2.LocationTypeAvailabilityZone),
			MaxResults:   aws.Int64(2),
			NextToken:    token,
		}, nc, 1, "", offeringsTestRegion, offeringsTestAZ)
		require.NoError(t, err)

		for _, offering := range output.InstanceTypeOfferings {
			seen[aws.StringValue(offering.InstanceType)]++
		}

		pages++
		require.Less(t, pages, 10, "traversal did not terminate")
		if output.NextToken == nil {
			break
		}
		assert.Len(t, output.InstanceTypeOfferings, 2, "a non-final page must be full")
		token = output.NextToken
	}

	assert.Equal(t, 3, pages)
	require.Len(t, seen, len(catalogue))
	for _, instanceType := range catalogue {
		assert.Equal(t, 1, seen[instanceType], "offering for %s appeared %d times", instanceType, seen[instanceType])
	}
}

func TestDescribeInstanceTypeOfferings_MalformedNextToken(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	serveInstanceTypes(t, nc, "t3.micro")

	_, err := DescribeInstanceTypeOfferings(context.Background(), &ec2.DescribeInstanceTypeOfferingsInput{
		LocationType: aws.String(ec2.LocationTypeAvailabilityZone),
		NextToken:    aws.String("not-a-token"),
	}, nc, 1, "", offeringsTestRegion, offeringsTestAZ)

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}

// TestDescribeInstanceTypeOfferings_TokenPastEnd guards the offset token against
// a caller replaying a stale page after the catalogue shrank.
func TestDescribeInstanceTypeOfferings_TokenPastEnd(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	serveInstanceTypes(t, nc, "t3.micro")

	output, err := DescribeInstanceTypeOfferings(context.Background(), &ec2.DescribeInstanceTypeOfferingsInput{
		LocationType: aws.String(ec2.LocationTypeAvailabilityZone),
		NextToken:    aws.String("99"),
	}, nc, 1, "", offeringsTestRegion, offeringsTestAZ)

	require.NoError(t, err)
	assert.Empty(t, output.InstanceTypeOfferings)
	assert.Nil(t, output.NextToken)
}

func TestDescribeInstanceTypeOfferings_EmptyCatalogue(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	output, err := DescribeInstanceTypeOfferings(context.Background(), &ec2.DescribeInstanceTypeOfferingsInput{
		LocationType: aws.String(ec2.LocationTypeAvailabilityZone),
	}, nc, 0, "", offeringsTestRegion, offeringsTestAZ)

	require.NoError(t, err)
	require.NotNil(t, output.InstanceTypeOfferings)
	assert.Empty(t, output.InstanceTypeOfferings)
	assert.Nil(t, output.NextToken)
}

func TestDescribeInstanceTypeOfferings_CatalogueError(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	closedNC, err := nats.Connect(nc.ConnectedUrl())
	require.NoError(t, err)
	closedNC.Close()

	_, err = DescribeInstanceTypeOfferings(context.Background(), &ec2.DescribeInstanceTypeOfferingsInput{
		LocationType: aws.String(ec2.LocationTypeAvailabilityZone),
	}, closedNC, 1, "", offeringsTestRegion, offeringsTestAZ)

	require.Error(t, err)
}

func TestDescribeInstanceTypeOfferings_NilInput(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	serveInstanceTypes(t, nc, "t3.micro")

	output, err := DescribeInstanceTypeOfferings(context.Background(), nil, nc, 1, "", offeringsTestRegion, offeringsTestAZ)

	require.NoError(t, err)
	require.Len(t, output.InstanceTypeOfferings, 1)
	assert.Equal(t, offeringsTestRegion, aws.StringValue(output.InstanceTypeOfferings[0].Location))
}
