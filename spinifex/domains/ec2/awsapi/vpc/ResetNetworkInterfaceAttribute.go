package vpc

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// ResetNetworkInterfaceAttribute resets sourceDestCheck to true. Modify refuses
// to disable it, so every interface already holds the default and a reset that
// finds the interface changes nothing.
func ResetNetworkInterfaceAttribute(ctx context.Context, input *ec2.ResetNetworkInterfaceAttributeInput, natsConn *nats.Conn, accountID string) (ec2.ResetNetworkInterfaceAttributeOutput, error) {
	var output ec2.ResetNetworkInterfaceAttributeOutput
	if input == nil {
		return output, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	// AWS takes the attribute as a bare flag and checks it before the interface.
	if input.SourceDestCheck == nil {
		return output, awserrors.Errorf(awserrors.ErrorInvalidParameterCombination, "No attributes specified.")
	}
	if *input.SourceDestCheck != "" {
		return output, awserrors.Errorf(awserrors.ErrorInvalidRequest, "The request received was invalid.")
	}
	if aws.StringValue(input.NetworkInterfaceId) == "" {
		return output, errors.New(awserrors.ErrorMissingParameter)
	}

	if _, err := DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{NetworkInterfaceIds: []*string{input.NetworkInterfaceId}}, natsConn, accountID); err != nil {
		return output, err
	}
	return output, nil
}
