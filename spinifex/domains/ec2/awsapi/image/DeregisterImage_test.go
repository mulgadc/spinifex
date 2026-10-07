package image

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
)

func TestValidateDeregisterImageInput(t *testing.T) {
	tests := []struct {
		name    string
		input   *ec2.DeregisterImageInput
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
			input:   &ec2.DeregisterImageInput{},
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
		},
		{
			name: "EmptyImageId",
			input: &ec2.DeregisterImageInput{
				ImageId: aws.String(""),
			},
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
		},
		{
			name: "MalformedImageId",
			input: &ec2.DeregisterImageInput{
				ImageId: aws.String("not-an-ami"),
			},
			wantErr: true,
			errMsg:  awserrors.ErrorInvalidAMIIDMalformed,
		},
		{
			name: "ValidImageId",
			input: &ec2.DeregisterImageInput{
				ImageId: aws.String("ami-1234567890abcdef0"),
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateDeregisterImageInput(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
				assert.Equal(t, tt.errMsg, err.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
