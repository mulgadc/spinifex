package gateway_ec2_image

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
)

func TestValidateResetImageAttributeInput(t *testing.T) {
	tests := []struct {
		name    string
		input   *ec2.ResetImageAttributeInput
		wantErr bool
		errMsg  string
	}{
		{
			name:    "NilInput",
			input:   nil,
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
		},
		{
			name:    "EmptyInput",
			input:   &ec2.ResetImageAttributeInput{},
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
		},
		{
			name: "MissingAttribute",
			input: &ec2.ResetImageAttributeInput{
				ImageId: aws.String("ami-1234567890abcdef0"),
			},
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
		},
		{
			name: "MalformedImageId",
			input: &ec2.ResetImageAttributeInput{
				ImageId:   aws.String("not-an-ami"),
				Attribute: aws.String("description"),
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidAMIIDMalformed,
		},
		{
			name: "UnsupportedAttribute_LaunchPermission",
			input: &ec2.ResetImageAttributeInput{
				ImageId:   aws.String("ami-1234567890abcdef0"),
				Attribute: aws.String("launchPermission"),
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "UnsupportedAttribute_BlockDeviceMapping",
			input: &ec2.ResetImageAttributeInput{
				ImageId:   aws.String("ami-1234567890abcdef0"),
				Attribute: aws.String("blockDeviceMapping"),
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "ValidDescription",
			input: &ec2.ResetImageAttributeInput{
				ImageId:   aws.String("ami-1234567890abcdef0"),
				Attribute: aws.String("description"),
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateResetImageAttributeInput(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
				assert.Equal(t, tt.errMsg, err.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestResetImageAttribute_GatewayValidationFailureReturnsEarly(t *testing.T) {
	_, err := ResetImageAttribute(context.Background(), &ec2.ResetImageAttributeInput{}, nil, "000000000001")
	assert.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}
