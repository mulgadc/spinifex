package key

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
)

func TestValidateImportKeyPairInput(t *testing.T) {
	tests := []struct {
		name  string
		input *ec2.ImportKeyPairInput
		want  error
	}{
		{
			name:  "NilInput",
			input: nil,
			want:  errors.New(awserrors.ErrorMissingParameter),
		},
		{
			name: "MissingKeyName",
			input: &ec2.ImportKeyPairInput{
				PublicKeyMaterial: []byte("ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQ..."),
			},
			want: errors.New(awserrors.ErrorMissingParameter),
		},
		{
			name: "MissingPublicKeyMaterial",
			input: &ec2.ImportKeyPairInput{
				KeyName: aws.String("test-key"),
			},
			want: errors.New(awserrors.ErrorMissingParameter),
		},
		{
			name: "EmptyPublicKeyMaterial",
			input: &ec2.ImportKeyPairInput{
				KeyName:           aws.String("test-key"),
				PublicKeyMaterial: []byte{},
			},
			want: errors.New(awserrors.ErrorMissingParameter),
		},
		{
			name: "ValidInput",
			input: &ec2.ImportKeyPairInput{
				KeyName:           aws.String("test-key"),
				PublicKeyMaterial: []byte("ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQ..."),
			},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test only the validation function (ImportKeyPair requires NATS infrastructure)
			err := ValidateImportKeyPairInput(tt.input)
			assert.Equal(t, tt.want, err)
		})
	}
}
