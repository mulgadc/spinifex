package gateway_ec2_zone

import (
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
)

func DescribeAvailabilityZones(input *ec2.DescribeAvailabilityZonesInput, region string, az string) (output *ec2.DescribeAvailabilityZonesOutput, err error) {
	output = &ec2.DescribeAvailabilityZonesOutput{
		AvailabilityZones: []*ec2.AvailabilityZone{
			{
				State:              aws.String("available"),
				OptInStatus:        aws.String("opt-in-not-required"),
				RegionName:         aws.String(region),
				ZoneName:           aws.String(az),
				ZoneId:             aws.String(handlers_ec2_vpc.SingleZoneID),
				GroupName:          aws.String(region),
				NetworkBorderGroup: aws.String(region),
				ZoneType:           aws.String("availability-zone"),
				Messages:           []*ec2.AvailabilityZoneMessage{},
			},
		},
	}

	return output, nil
}

func DescribeRegions(input *ec2.DescribeRegionsInput, region string) (output *ec2.DescribeRegionsOutput, err error) {
	output = &ec2.DescribeRegionsOutput{
		Regions: []*ec2.Region{
			{
				Endpoint:    aws.String("https://localhost:9999"),
				RegionName:  aws.String(region),
				OptInStatus: aws.String("opt-in-not-required"),
			},
		},
	}

	return output, nil
}
