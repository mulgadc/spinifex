package volume

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
)

func TestValidateDescribeVolumesModificationsInput(t *testing.T) {
	tests := []struct {
		name    string
		input   *ec2.DescribeVolumesModificationsInput
		wantErr bool
		errMsg  string
	}{
		{
			name:    "NilInput",
			input:   nil,
			wantErr: false,
		},
		{
			name:    "EmptyInput",
			input:   &ec2.DescribeVolumesModificationsInput{},
			wantErr: false,
		},
		{
			name: "ValidVolumeId",
			input: &ec2.DescribeVolumesModificationsInput{
				VolumeIds: []*string{aws.String("vol-abc123")},
			},
			wantErr: false,
		},
		{
			name: "NilVolumeIdEntry",
			input: &ec2.DescribeVolumesModificationsInput{
				VolumeIds: []*string{nil, aws.String("vol-abc123")},
			},
			wantErr: false,
		},
		{
			name: "InvalidVolumeId_WrongPrefix",
			input: &ec2.DescribeVolumesModificationsInput{
				VolumeIds: []*string{aws.String("ami-0123")},
			},
			wantErr: true,
			errMsg:  "InvalidVolumeID.Malformed",
		},
		{
			name: "InvalidVolumeId_EmptyString",
			input: &ec2.DescribeVolumesModificationsInput{
				VolumeIds: []*string{aws.String("")},
			},
			wantErr: true,
			errMsg:  "InvalidVolumeID.Malformed",
		},
		{
			name: "MixedValidAndInvalid",
			input: &ec2.DescribeVolumesModificationsInput{
				VolumeIds: []*string{aws.String("vol-valid"), aws.String("bogus")},
			},
			wantErr: true,
			errMsg:  "InvalidVolumeID.Malformed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateDescribeVolumesModificationsInput(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
				assert.Equal(t, tt.errMsg, err.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
