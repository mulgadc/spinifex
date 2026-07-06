package gateway_ec2_image

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateModifyImageAttributeInput(t *testing.T) {
	tests := []struct {
		name    string
		input   *ec2.ModifyImageAttributeInput
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
			name: "MissingImageId",
			input: &ec2.ModifyImageAttributeInput{
				Description: &ec2.AttributeValue{Value: aws.String("hi")},
			},
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
		},
		{
			name: "MalformedImageId",
			input: &ec2.ModifyImageAttributeInput{
				ImageId:     aws.String("not-an-ami"),
				Description: &ec2.AttributeValue{Value: aws.String("hi")},
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidAMIIDMalformed,
		},
		{
			name: "NoAttributeSet",
			input: &ec2.ModifyImageAttributeInput{
				ImageId: aws.String("ami-1234567890abcdef0"),
			},
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
		},
		{
			name: "BothDescriptionAndAttributeSet",
			input: &ec2.ModifyImageAttributeInput{
				ImageId:     aws.String("ami-1234567890abcdef0"),
				Description: &ec2.AttributeValue{Value: aws.String("hi")},
				Attribute:   aws.String("description"),
				Value:       aws.String("hi"),
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterCombination,
		},
		{
			name: "LaunchPermissionRejected",
			input: &ec2.ModifyImageAttributeInput{
				ImageId:          aws.String("ami-1234567890abcdef0"),
				LaunchPermission: &ec2.LaunchPermissionModifications{},
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "ImdsSupportRejected",
			input: &ec2.ModifyImageAttributeInput{
				ImageId:     aws.String("ami-1234567890abcdef0"),
				ImdsSupport: &ec2.AttributeValue{Value: aws.String("v2.0")},
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "ProductCodesRejected",
			input: &ec2.ModifyImageAttributeInput{
				ImageId:      aws.String("ami-1234567890abcdef0"),
				ProductCodes: []*string{aws.String("abc")},
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "UserIdsRejected",
			input: &ec2.ModifyImageAttributeInput{
				ImageId: aws.String("ami-1234567890abcdef0"),
				UserIds: []*string{aws.String("123456789012")},
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "UnsupportedAttribute_BootMode",
			input: &ec2.ModifyImageAttributeInput{
				ImageId:   aws.String("ami-1234567890abcdef0"),
				Attribute: aws.String("bootMode"),
				Value:     aws.String("uefi"),
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "UnsupportedAttribute_LaunchPermission",
			input: &ec2.ModifyImageAttributeInput{
				ImageId:   aws.String("ami-1234567890abcdef0"),
				Attribute: aws.String("launchPermission"),
				Value:     aws.String("foo"),
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "ValidTopLevelDescription",
			input: &ec2.ModifyImageAttributeInput{
				ImageId:     aws.String("ami-1234567890abcdef0"),
				Description: &ec2.AttributeValue{Value: aws.String("updated")},
			},
			wantErr: false,
		},
		{
			name: "ValidStructuredDescription",
			input: &ec2.ModifyImageAttributeInput{
				ImageId:   aws.String("ami-1234567890abcdef0"),
				Attribute: aws.String("description"),
				Value:     aws.String("updated"),
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateModifyImageAttributeInput(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
				assert.Equal(t, tt.errMsg, err.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestValidateModifyImageAttributeInput_NormalizesTopLevelDescription(t *testing.T) {
	input := &ec2.ModifyImageAttributeInput{
		ImageId:     aws.String("ami-1234567890abcdef0"),
		Description: &ec2.AttributeValue{Value: aws.String("new-desc")},
	}
	require.NoError(t, ValidateModifyImageAttributeInput(input))
	require.NotNil(t, input.Attribute)
	assert.Equal(t, "description", *input.Attribute)
	require.NotNil(t, input.Value)
	assert.Equal(t, "new-desc", *input.Value)
}

func TestValidateModifyImageAttributeInput_NormalizesEmptyDescriptionToEmptyString(t *testing.T) {
	// --description Value="" should clear the field, not error.
	input := &ec2.ModifyImageAttributeInput{
		ImageId:     aws.String("ami-1234567890abcdef0"),
		Description: &ec2.AttributeValue{Value: aws.String("")},
	}
	require.NoError(t, ValidateModifyImageAttributeInput(input))
	require.NotNil(t, input.Value)
	assert.Equal(t, "", *input.Value)
}

func TestModifyImageAttribute_GatewayValidationFailureReturnsEarly(t *testing.T) {
	_, err := ModifyImageAttribute(context.Background(), &ec2.ModifyImageAttributeInput{}, nil, "000000000001")
	assert.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}
