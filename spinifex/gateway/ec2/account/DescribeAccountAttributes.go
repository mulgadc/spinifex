package gateway_ec2_account

import (
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
)

// DescribeAccountAttributes returns static account attributes for the Spinifex platform.
func DescribeAccountAttributes(input *ec2.DescribeAccountAttributesInput) (*ec2.DescribeAccountAttributesOutput, error) {
	requestedAttrs := make(map[string]bool)
	for _, name := range input.AttributeNames {
		if name != nil {
			requestedAttrs[*name] = true
		}
	}

	returnAll := len(requestedAttrs) == 0

	var accountAttributes []*ec2.AccountAttribute

	if returnAll || requestedAttrs["supported-platforms"] {
		accountAttributes = append(accountAttributes, &ec2.AccountAttribute{
			AttributeName: aws.String("supported-platforms"),
			AttributeValues: []*ec2.AccountAttributeValue{
				{AttributeValue: aws.String("VPC")},
			},
		})
	}

	if returnAll || requestedAttrs["default-vpc"] {
		accountAttributes = append(accountAttributes, &ec2.AccountAttribute{
			AttributeName: aws.String("default-vpc"),
			AttributeValues: []*ec2.AccountAttributeValue{
				{AttributeValue: aws.String("none")},
			},
		})
	}

	if returnAll || requestedAttrs["max-instances"] {
		accountAttributes = append(accountAttributes, &ec2.AccountAttribute{
			AttributeName: aws.String("max-instances"),
			AttributeValues: []*ec2.AccountAttributeValue{
				{AttributeValue: aws.String("100")},
			},
		})
	}

	if returnAll || requestedAttrs["vpc-max-security-groups-per-interface"] {
		accountAttributes = append(accountAttributes, &ec2.AccountAttribute{
			AttributeName: aws.String("vpc-max-security-groups-per-interface"),
			AttributeValues: []*ec2.AccountAttributeValue{
				{AttributeValue: aws.String("5")},
			},
		})
	}

	if returnAll || requestedAttrs["max-elastic-ips"] {
		accountAttributes = append(accountAttributes, &ec2.AccountAttribute{
			AttributeName: aws.String("max-elastic-ips"),
			AttributeValues: []*ec2.AccountAttributeValue{
				{AttributeValue: aws.String("5")},
			},
		})
	}

	if returnAll || requestedAttrs["vpc-max-elastic-ips"] {
		accountAttributes = append(accountAttributes, &ec2.AccountAttribute{
			AttributeName: aws.String("vpc-max-elastic-ips"),
			AttributeValues: []*ec2.AccountAttributeValue{
				{AttributeValue: aws.String("20")},
			},
		})
	}

	output := &ec2.DescribeAccountAttributesOutput{
		AccountAttributes: accountAttributes,
	}

	slog.Info("DescribeAccountAttributes completed", "attributeCount", len(accountAttributes))
	return output, nil
}
