package vpc

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// DescribeNetworkInterfaceAttribute projects one attribute of the interface
// DescribeNetworkInterfaces returns for the id.
func DescribeNetworkInterfaceAttribute(ctx context.Context, input *ec2.DescribeNetworkInterfaceAttributeInput, natsConn *nats.Conn, accountID string) (ec2.DescribeNetworkInterfaceAttributeOutput, error) {
	var output ec2.DescribeNetworkInterfaceAttributeOutput
	if input == nil || aws.StringValue(input.NetworkInterfaceId) == "" || aws.StringValue(input.Attribute) == "" {
		return output, errors.New(awserrors.ErrorMissingParameter)
	}
	attribute := aws.StringValue(input.Attribute)
	switch attribute {
	case ec2.NetworkInterfaceAttributeAttachment, ec2.NetworkInterfaceAttributeDescription,
		ec2.NetworkInterfaceAttributeGroupSet, ec2.NetworkInterfaceAttributeSourceDestCheck:
	default:
		return output, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	enis, err := DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{NetworkInterfaceIds: []*string{input.NetworkInterfaceId}}, natsConn, accountID)
	if err != nil {
		return output, err
	}
	if len(enis.NetworkInterfaces) == 0 {
		return output, errors.New(awserrors.ErrorInvalidNetworkInterfaceIDNotFound)
	}
	eni := enis.NetworkInterfaces[0]

	output.NetworkInterfaceId = eni.NetworkInterfaceId
	switch attribute {
	case ec2.NetworkInterfaceAttributeAttachment:
		output.Attachment = eni.Attachment
	case ec2.NetworkInterfaceAttributeDescription:
		output.Description = &ec2.AttributeValue{Value: eni.Description}
	case ec2.NetworkInterfaceAttributeGroupSet:
		output.Groups = eni.Groups
	case ec2.NetworkInterfaceAttributeSourceDestCheck:
		output.SourceDestCheck = &ec2.AttributeBooleanValue{Value: eni.SourceDestCheck}
	}
	return output, nil
}
