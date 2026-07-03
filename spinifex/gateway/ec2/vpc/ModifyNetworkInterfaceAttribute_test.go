package gateway_ec2_vpc

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
)

func TestModifyNetworkInterfaceAttribute_NilInput(t *testing.T) {
	_, err := ModifyNetworkInterfaceAttribute(nil, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestModifyNetworkInterfaceAttribute_NilNetworkInterfaceId(t *testing.T) {
	_, err := ModifyNetworkInterfaceAttribute(&ec2.ModifyNetworkInterfaceAttributeInput{}, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestModifyNetworkInterfaceAttribute_EmptyNetworkInterfaceId(t *testing.T) {
	_, err := ModifyNetworkInterfaceAttribute(&ec2.ModifyNetworkInterfaceAttributeInput{
		NetworkInterfaceId: aws.String(""),
	}, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestModifyNetworkInterfaceAttribute_NoAttributes(t *testing.T) {
	_, err := ModifyNetworkInterfaceAttribute(&ec2.ModifyNetworkInterfaceAttributeInput{
		NetworkInterfaceId: aws.String("eni-abc123"),
	}, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestModifyNetworkInterfaceAttribute_SourceDestCheckOnly_PassesValidation(t *testing.T) {
	err := ValidateModifyNetworkInterfaceAttributeInput(&ec2.ModifyNetworkInterfaceAttributeInput{
		NetworkInterfaceId: aws.String("eni-abc123"),
		SourceDestCheck:    &ec2.AttributeBooleanValue{Value: aws.Bool(false)},
	})
	assert.NoError(t, err)
}

func TestModifyNetworkInterfaceAttribute_NilNATS(t *testing.T) {
	_, err := ModifyNetworkInterfaceAttribute(&ec2.ModifyNetworkInterfaceAttributeInput{
		NetworkInterfaceId: aws.String("eni-abc123"),
		Description:        &ec2.AttributeValue{Value: aws.String("desc")},
	}, nil, "123456789012")
	assert.Error(t, err)
}
