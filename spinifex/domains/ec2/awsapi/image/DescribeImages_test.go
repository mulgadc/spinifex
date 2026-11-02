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
					aws.String("ami-1a2b3c4d"),
					aws.String("ami-0123456789abcdef1"),
				},
			},
			wantErr: false,
		},
		{
			name: "MixedValidAndInvalidImageIds",
			input: &ec2.DescribeImagesInput{
				ImageIds: []*string{
					aws.String("ami-0123456789abcdef0"),
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
				code, ok := awserrors.ResolveErrorCode(err)
				assert.True(t, ok)
				assert.Equal(t, tt.errMsg, code)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// AWS answers a well-prefixed ID whose suffix is not 8 or 17 lowercase hex
// characters with Malformed, not NotFound.
func TestValidateDescribeImagesInput_MalformedSuffix(t *testing.T) {
	for _, id := range []string{"ami-zzz", "ami-0123456789ABCDEF0", "ami-0123456789abcdef", "ami-"} {
		err := ValidateDescribeImagesInput(&ec2.DescribeImagesInput{ImageIds: []*string{aws.String(id)}})
		code, msg, ok := awserrors.ResolveErrorDetail(err)
		assert.True(t, ok, id)
		assert.Equal(t, awserrors.ErrorInvalidAMIIDMalformed, code, id)
		assert.Equal(t, `Invalid id: "`+id+`"`, msg, id)
	}
}
