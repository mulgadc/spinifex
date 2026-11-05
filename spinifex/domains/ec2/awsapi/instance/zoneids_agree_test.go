package instance

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	ec2zoneapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/zone"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/zoneid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// With a non-default override in force, the three calls that report a zone ID
// read one source and so cannot disagree.
func TestZoneIDs_ThreeProducersAgreeUnderAnOverride(t *testing.T) {
	const region, az, account = "mulga-lab", "mulga-laba", "123456789012"
	_, nc, js := testutil.StartTestJetStream(t)
	testutil.StubVpcdSGResponder(t, nc)
	serveInstanceTypes(t, nc, "t3.micro")

	ids, err := zoneid.NewResolver(js, region, "ov99")
	require.NoError(t, err)

	vpcSvc, err := ec2vpc.NewVPCServiceImplWithNATS(t.Context(), nil, nc)
	require.NoError(t, err)
	vpcSvc.SetZoneIDSource(ids)
	vpc, err := vpcSvc.CreateVpc(t.Context(), &ec2.CreateVpcInput{CidrBlock: aws.String("10.0.0.0/16")}, account)
	require.NoError(t, err)
	_, err = vpcSvc.CreateSubnet(t.Context(), &ec2.CreateSubnetInput{
		VpcId: vpc.Vpc.VpcId, CidrBlock: aws.String("10.0.1.0/24"), AvailabilityZone: aws.String(az),
	}, account)
	require.NoError(t, err)

	zones, err := ec2zoneapi.DescribeAvailabilityZones(t.Context(), &ec2.DescribeAvailabilityZonesInput{}, region, az, ids)
	require.NoError(t, err)
	require.Len(t, zones.AvailabilityZones, 1)
	fromZones := aws.StringValue(zones.AvailabilityZones[0].ZoneId)

	subnets, err := vpcSvc.DescribeSubnets(t.Context(), &ec2.DescribeSubnetsInput{}, account)
	require.NoError(t, err)
	var fromSubnet string
	for _, s := range subnets.Subnets {
		if aws.StringValue(s.AvailabilityZone) == az {
			fromSubnet = aws.StringValue(s.AvailabilityZoneId)
		}
	}

	offerings, err := DescribeInstanceTypeOfferings(t.Context(), &ec2.DescribeInstanceTypeOfferingsInput{
		LocationType: aws.String(ec2.LocationTypeAvailabilityZoneId),
	}, nc, 1, account, region, az, ids)
	require.NoError(t, err)
	require.NotEmpty(t, offerings.InstanceTypeOfferings)
	fromOfferings := aws.StringValue(offerings.InstanceTypeOfferings[0].Location)

	assert.Equal(t, "ov99-az1", fromZones)
	assert.Equal(t, fromZones, fromSubnet)
	assert.Equal(t, fromZones, fromOfferings)
}
