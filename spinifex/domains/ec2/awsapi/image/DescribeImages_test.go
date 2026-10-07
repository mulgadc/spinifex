package image

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
)

func TestValidateDescribeImagesInput(t *testing.T) {
	tests := []struct {
		name    string
		input   *ec2.DescribeImagesInput
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
			input:   &ec2.DescribeImagesInput{},
			wantErr: false,
		},
		{
			name: "ValidImageId",
			input: &ec2.DescribeImagesInput{
				ImageIds: []*string{aws.String("ami-0123456789abcdef0")},
			},
			wantErr: false,
		},
		{
			name: "InvalidImageId_NoPrefix",
			input: &ec2.DescribeImagesInput{
				ImageIds: []*string{aws.String("invalid-id")},
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidAMIIDMalformed,
		},
		{
			name: "InvalidImageId_WrongPrefix",
			input: &ec2.DescribeImagesInput{
				ImageIds: []*string{aws.String("vol-0123456789abcdef0")},
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidAMIIDMalformed,
		},
		{
			name: "MultipleValidImageIds",
			input: &ec2.DescribeImagesInput{
				ImageIds: []*string{
					aws.String("ami-111"),
					aws.String("ami-222"),
				},
			},
			wantErr: false,
		},
		{
			name: "MixedValidAndInvalidImageIds",
			input: &ec2.DescribeImagesInput{
				ImageIds: []*string{
					aws.String("ami-valid"),
					aws.String("invalid-id"),
				},
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidAMIIDMalformed,
		},
		{
			name: "EmptyImageId",
			input: &ec2.DescribeImagesInput{
				ImageIds: []*string{aws.String("")},
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidAMIIDMalformed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateDescribeImagesInput(tt.input)

			if tt.wantErr {
				assert.Error(t, err)
				assert.Equal(t, tt.errMsg, err.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
