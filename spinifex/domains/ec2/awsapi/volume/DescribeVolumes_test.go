package volume

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
)

func TestValidateDescribeVolumesInput(t *testing.T) {
	tests := []struct {
		name    string
		input   *ec2.DescribeVolumesInput
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
			input:   &ec2.DescribeVolumesInput{},
			wantErr: false,
		},
		{
			name: "ValidSingleVolumeId",
			input: &ec2.DescribeVolumesInput{
				VolumeIds: []*string{aws.String("vol-0123456789abcdef0")},
			},
			wantErr: false,
		},
		{
			name: "ValidMultipleVolumeIds",
			input: &ec2.DescribeVolumesInput{
				VolumeIds: []*string{
					aws.String("vol-0123456789abcdef1"),
					aws.String("vol-1a2b3c4d"),
					aws.String("vol-0123456789abcdef2"),
				},
			},
			wantErr: false,
		},
		{
			name: "InvalidVolumeId_NoPrefix",
			input: &ec2.DescribeVolumesInput{
				VolumeIds: []*string{aws.String("invalid-id")},
			},
			wantErr: true,
			errMsg:  "InvalidVolumeID.Malformed",
		},
		{
			name: "InvalidVolumeId_WrongPrefix",
			input: &ec2.DescribeVolumesInput{
				VolumeIds: []*string{aws.String("ami-0123456789abcdef0")},
			},
			wantErr: true,
			errMsg:  "InvalidVolumeID.Malformed",
		},
		{
			name: "InvalidVolumeId_EmptyString",
			input: &ec2.DescribeVolumesInput{
				VolumeIds: []*string{aws.String("")},
			},
			wantErr: true,
			errMsg:  "InvalidVolumeID.Malformed",
		},
		{
			name: "MixedValidAndInvalid",
			input: &ec2.DescribeVolumesInput{
				VolumeIds: []*string{
					aws.String("vol-0123456789abcdef3"),
					aws.String("invalid-id"),
				},
			},
			wantErr: true,
			errMsg:  "InvalidVolumeID.Malformed",
		},
		{
			name: "NilVolumeIdInList",
			input: &ec2.DescribeVolumesInput{
				VolumeIds: []*string{nil, aws.String("vol-0123456789abcdef1")},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateDescribeVolumesInput(tt.input)

			if tt.wantErr {
				assert.Error(t, err)
				assert.Equal(t, tt.errMsg, err.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// AWS answers a well-prefixed ID whose suffix is not 8 or 17 lowercase hex
// characters with InvalidParameterValue, not NotFound.
func TestValidateDescribeVolumesInput_MalformedSuffix(t *testing.T) {
	for _, id := range []string{"vol-zzz", "vol-0123456789ABCDEF0", "vol-0123456789abcdef", "vol-"} {
		err := ValidateDescribeVolumesInput(&ec2.DescribeVolumesInput{VolumeIds: []*string{aws.String(id)}})
		code, msg, ok := awserrors.ResolveErrorDetail(err)
		assert.True(t, ok, id)
		assert.Equal(t, awserrors.ErrorInvalidParameterValue, code, id)
		assert.Equal(t, "Value ("+id+") for parameter volumes is invalid. Expected: 'vol-...'.", msg, id)
	}
}
