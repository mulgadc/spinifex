package key

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2key "github.com/mulgadc/spinifex/spinifex/domains/ec2/key"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// ValidateImportKeyPairInput returns MissingParameter for a nil input, an empty KeyName or empty
// PublicKeyMaterial.
func ValidateImportKeyPairInput(input *ec2.ImportKeyPairInput) (err error) {
	if input == nil {
		return errors.New(awserrors.ErrorMissingParameter)
	}

	if input.KeyName == nil || *input.KeyName == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}

	if len(input.PublicKeyMaterial) == 0 {
		return errors.New(awserrors.ErrorMissingParameter)
	}

	return err
}

// ImportKeyPair implements the EC2 ImportKeyPair action, storing a caller-supplied public key for
// accountID via the NATS key service.
func ImportKeyPair(ctx context.Context, input *ec2.ImportKeyPairInput, natsConn *nats.Conn, accountID string) (output ec2.ImportKeyPairOutput, err error) {
	err = ValidateImportKeyPairInput(input)

	if err != nil {
		return output, err
	}

	keyService := ec2key.NewNATSKeyService(natsConn)
	result, err := keyService.ImportKeyPair(ctx, input, accountID)

	if err != nil {
		return output, err
	}

	output = *result
	return output, nil
}
