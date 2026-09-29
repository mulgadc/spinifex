package gateway_ec2_image

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
)

func TestValidateCreateImageInput(t *testing.T) {
	tests := []struct {
		name    string
		input   *ec2.CreateImageInput
		wantErr bool
		errMsg  string
	}{
		{
			name:    "NilInput",
			input:   nil,
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name:    "EmptyInput",
			input:   &ec2.CreateImageInput{},
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
		},
		{
			name: "MissingName",
			input: &ec2.CreateImageInput{
				InstanceId: aws.String("i-1234567890abcdef0"),
			},
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
		},
		{
			name: "InvalidInstanceIdFormat",
			input: &ec2.CreateImageInput{
				InstanceId: aws.String("invalid-id"),
				Name:       aws.String("my-image"),
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidInstanceIDMalformed,
		},
		{
			name: "EmptyInstanceId",
			input: &ec2.CreateImageInput{
				InstanceId: aws.String(""),
				Name:       aws.String("my-image"),
			},
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
		},
		{
			name: "EmptyName",
			input: &ec2.CreateImageInput{
				InstanceId: aws.String("i-1234567890abcdef0"),
				Name:       aws.String(""),
			},
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
		},
		{
			name: "ValidInput",
			input: &ec2.CreateImageInput{
				InstanceId: aws.String("i-1234567890abcdef0"),
				Name:       aws.String("my-image"),
			},
			wantErr: false,
		},
		{
			name: "ValidInputWithDescription",
			input: &ec2.CreateImageInput{
				InstanceId:  aws.String("i-1234567890abcdef0"),
				Name:        aws.String("my-image"),
				Description: aws.String("A snapshot image"),
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateCreateImageInput(tt.input)

			if tt.wantErr {
				assert.Error(t, err)
				assert.Equal(t, tt.errMsg, err.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
