package key

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
)

func TestValidateCreateKeyPairInput(t *testing.T) {
	tests := []struct {
		name    string
		input   *ec2.CreateKeyPairInput
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
			name: "MissingKeyName",
			input: &ec2.CreateKeyPairInput{
				KeyName: aws.String(""),
			},
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
		},
		{
			name:    "NilKeyName",
			input:   &ec2.CreateKeyPairInput{},
			wantErr: true,
			errMsg:  awserrors.ErrorMissingParameter,
		},
		{
			name: "ValidInput",
			input: &ec2.CreateKeyPairInput{
				KeyName: aws.String("test-key"),
			},
			wantErr: false,
		},
		{
			name: "ValidInputWithKeyType",
			input: &ec2.CreateKeyPairInput{
				KeyName: aws.String("test-key"),
				KeyType: aws.String("rsa"),
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateCreateKeyPairInput(tt.input)

			if tt.wantErr {
				assert.Error(t, err)
				assert.Equal(t, tt.errMsg, err.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
