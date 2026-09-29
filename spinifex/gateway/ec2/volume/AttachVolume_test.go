package gateway_ec2_volume

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
)

func TestValidateAttachVolumeInput(t *testing.T) {
	tests := []struct {
		name    string
		input   *ec2.AttachVolumeInput
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
			input:   &ec2.AttachVolumeInput{},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "MissingVolumeId",
			input: &ec2.AttachVolumeInput{
				InstanceId: aws.String("i-1234567890abcdef0"),
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "MissingInstanceId",
			input: &ec2.AttachVolumeInput{
				VolumeId: aws.String("vol-1234567890abcdef0"),
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "EmptyVolumeId",
			input: &ec2.AttachVolumeInput{
				VolumeId:   aws.String(""),
				InstanceId: aws.String("i-1234567890abcdef0"),
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "EmptyInstanceId",
			input: &ec2.AttachVolumeInput{
				VolumeId:   aws.String("vol-1234567890abcdef0"),
				InstanceId: aws.String(""),
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "ValidInput_NoDevice",
			input: &ec2.AttachVolumeInput{
				VolumeId:   aws.String("vol-1234567890abcdef0"),
				InstanceId: aws.String("i-1234567890abcdef0"),
			},
			wantErr: false,
		},
		{
			name: "ValidInput_WithDevice",
			input: &ec2.AttachVolumeInput{
				VolumeId:   aws.String("vol-1234567890abcdef0"),
				InstanceId: aws.String("i-1234567890abcdef0"),
				Device:     aws.String("/dev/sdf"),
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateAttachVolumeInput(tt.input)

			if tt.wantErr {
				assert.Error(t, err)
				assert.Equal(t, tt.errMsg, err.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
