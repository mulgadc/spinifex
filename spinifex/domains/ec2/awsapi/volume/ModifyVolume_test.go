package volume

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
)

func TestValidateModifyVolumeInput(t *testing.T) {
	tests := []struct {
		name    string
		input   *ec2.ModifyVolumeInput
		wantErr bool
		errMsg  string
	}{
		{
			name:    "NilInput",
			input:   nil,
			wantErr: true,
			errMsg:  "InvalidParameterValue",
		},
		{
			name:    "EmptyInput_NoVolumeId",
			input:   &ec2.ModifyVolumeInput{},
			wantErr: true,
			errMsg:  "InvalidVolumeID.Malformed",
		},
		{
			name: "ValidVolumeId_WithSize",
			input: &ec2.ModifyVolumeInput{
				VolumeId: aws.String("vol-0123456789abcdef0"),
				Size:     aws.Int64(64),
			},
			wantErr: false,
		},
		{
			name: "ValidVolumeId_NoSize",
			input: &ec2.ModifyVolumeInput{
				VolumeId: aws.String("vol-abc123"),
			},
			wantErr: false,
		},
		{
			name: "InvalidVolumeId_NoPrefix",
			input: &ec2.ModifyVolumeInput{
				VolumeId: aws.String("invalid-id"),
				Size:     aws.Int64(64),
			},
			wantErr: true,
			errMsg:  "InvalidVolumeID.Malformed",
		},
		{
			name: "InvalidVolumeId_WrongPrefix",
			input: &ec2.ModifyVolumeInput{
				VolumeId: aws.String("ami-0123456789abcdef0"),
				Size:     aws.Int64(64),
			},
			wantErr: true,
			errMsg:  "InvalidVolumeID.Malformed",
		},
		{
			name: "InvalidVolumeId_EmptyString",
			input: &ec2.ModifyVolumeInput{
				VolumeId: aws.String(""),
				Size:     aws.Int64(64),
			},
			wantErr: true,
			errMsg:  "InvalidVolumeID.Malformed",
		},
		{
			name: "InvalidVolumeId_Nil",
			input: &ec2.ModifyVolumeInput{
				VolumeId: nil,
				Size:     aws.Int64(64),
			},
			wantErr: true,
			errMsg:  "InvalidVolumeID.Malformed",
		},
		{
			name: "InvalidSize_Zero",
			input: &ec2.ModifyVolumeInput{
				VolumeId: aws.String("vol-abc123"),
				Size:     aws.Int64(0),
			},
			wantErr: true,
			errMsg:  "InvalidParameterValue",
		},
		{
			name: "InvalidSize_Negative",
			input: &ec2.ModifyVolumeInput{
				VolumeId: aws.String("vol-abc123"),
				Size:     aws.Int64(-10),
			},
			wantErr: true,
			errMsg:  "InvalidParameterValue",
		},
		{
			name: "ValidWithVolumeType",
			input: &ec2.ModifyVolumeInput{
				VolumeId:   aws.String("vol-abc123"),
				Size:       aws.Int64(100),
				VolumeType: aws.String("gp3"),
			},
			wantErr: false,
		},
		{
			name: "ValidWithIops",
			input: &ec2.ModifyVolumeInput{
				VolumeId: aws.String("vol-abc123"),
				Size:     aws.Int64(100),
				Iops:     aws.Int64(3000),
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateModifyVolumeInput(tt.input)

			if tt.wantErr {
				assert.Error(t, err)
				assert.Equal(t, tt.errMsg, err.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
