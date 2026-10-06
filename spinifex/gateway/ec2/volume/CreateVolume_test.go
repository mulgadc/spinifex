package gateway_ec2_volume

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateCreateVolumeInput(t *testing.T) {
	tests := []struct {
		name    string
		input   *ec2.CreateVolumeInput
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
			input:   &ec2.CreateVolumeInput{},
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
		},
		{
			name: "ValidInput",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(80),
				AvailabilityZone: aws.String("ap-southeast-2a"),
			},
			wantErr: false,
		},
		{
			name: "ValidInput_WithGP3",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(80),
				AvailabilityZone: aws.String("ap-southeast-2a"),
				VolumeType:       aws.String("gp3"),
			},
			wantErr: false,
		},
		{
			name: "InvalidSize_Zero",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(0),
				AvailabilityZone: aws.String("ap-southeast-2a"),
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "InvalidSize_Negative",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(-10),
				AvailabilityZone: aws.String("ap-southeast-2a"),
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "InvalidSize_NilWithoutSnapshot",
			input: &ec2.CreateVolumeInput{
				AvailabilityZone: aws.String("ap-southeast-2a"),
			},
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
		},
		{
			name: "ValidInput_NilSizeWithSnapshot",
			input: &ec2.CreateVolumeInput{
				AvailabilityZone: aws.String("ap-southeast-2a"),
				SnapshotId:       aws.String("snap-12345678"),
			},
			wantErr: false,
		},
		{
			name: "InvalidSize_ZeroWithSnapshot",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(0),
				AvailabilityZone: aws.String("ap-southeast-2a"),
				SnapshotId:       aws.String("snap-12345678"),
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "InvalidSize_NilWithEmptySnapshot",
			input: &ec2.CreateVolumeInput{
				AvailabilityZone: aws.String("ap-southeast-2a"),
				SnapshotId:       aws.String(""),
			},
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
		},
		{
			name: "InvalidSize_TooLarge",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(16385),
				AvailabilityZone: aws.String("ap-southeast-2a"),
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "InvalidAZ_Empty",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(80),
				AvailabilityZone: aws.String(""),
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "InvalidAZ_Nil",
			input: &ec2.CreateVolumeInput{
				Size: aws.Int64(80),
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "InvalidVolumeType_IO1",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(80),
				AvailabilityZone: aws.String("ap-southeast-2a"),
				VolumeType:       aws.String("io1"),
			},
			wantErr: true,
			errMsg:  awserrors.ErrorUnknownVolumeType,
		},
		{
			name: "InvalidVolumeType_GP2",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(80),
				AvailabilityZone: aws.String("ap-southeast-2a"),
				VolumeType:       aws.String("gp2"),
			},
			wantErr: true,
			errMsg:  awserrors.ErrorUnknownVolumeType,
		},
		{
			name: "ValidInput_EmptyVolumeType",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(80),
				AvailabilityZone: aws.String("ap-southeast-2a"),
				VolumeType:       aws.String(""),
			},
			wantErr: false,
		},
		{
			name: "ValidInput_UppercaseGP3",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(80),
				AvailabilityZone: aws.String("ap-southeast-2a"),
				VolumeType:       aws.String("GP3"),
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateCreateVolumeInput(tt.input)

			if tt.wantErr {
				code, _, ok := awserrors.ResolveErrorDetail(err)
				require.True(t, ok)
				assert.Equal(t, tt.errMsg, code)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
