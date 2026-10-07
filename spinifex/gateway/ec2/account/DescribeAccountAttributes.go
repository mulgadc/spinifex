// Package gateway_ec2_account implements the EC2 account-level actions: it
// answers DescribeAccountAttributes itself and forwards the EBS-encryption and
// serial-console defaults to the account settings service over NATS.
package gateway_ec2_account

import (
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
)

// DescribeAccountAttributes returns the account's attributes. Every attribute
// but default-vpc is static; defaultVPCID looks that one up, is called only
// when it is asked for, and returns "" for an account with no default VPC.
func DescribeAccountAttributes(input *ec2.DescribeAccountAttributesInput, defaultVPCID func() (string, error)) (*ec2.DescribeAccountAttributesOutput, error) {
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
		vpcID, err := defaultVPCID()
		if err != nil {
			return nil, err
		}
		if vpcID == "" {
			vpcID = "none"
		}
		accountAttributes = append(accountAttributes, &ec2.AccountAttribute{
			AttributeName: aws.String("default-vpc"),
			AttributeValues: []*ec2.AccountAttributeValue{
				{AttributeValue: aws.String(vpcID)},
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
