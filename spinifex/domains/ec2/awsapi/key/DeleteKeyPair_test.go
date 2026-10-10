package key

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
)

func TestValidateDeleteKeyPairInput(t *testing.T) {
	tests := []struct {
		name    string
		input   *ec2.DeleteKeyPairInput
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
			name: "MissingBothKeyNameAndKeyPairId",
			input: &ec2.DeleteKeyPairInput{
				KeyName:   aws.String(""),
				KeyPairId: aws.String(""),
			},
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
		},
		{
			name:    "EmptyInput",
			input:   &ec2.DeleteKeyPairInput{},
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
		},
		{
			name: "ValidInputWithKeyName",
			input: &ec2.DeleteKeyPairInput{
				KeyName: aws.String("test-key"),
			},
			wantErr: false,
		},
		{
			name: "ValidInputWithKeyPairId",
			input: &ec2.DeleteKeyPairInput{
				KeyPairId: aws.String("key-0123456789abcdef0"),
			},
			wantErr: false,
		},
		{
			name: "ValidInputWithBoth",
			input: &ec2.DeleteKeyPairInput{
				KeyName:   aws.String("test-key"),
				KeyPairId: aws.String("key-0123456789abcdef0"),
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateDeleteKeyPairInput(tt.input)

			if tt.wantErr {
				assert.Error(t, err)
				assert.Equal(t, tt.errMsg, err.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
