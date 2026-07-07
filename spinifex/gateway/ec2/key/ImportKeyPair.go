package gateway_ec2_key

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_key "github.com/mulgadc/spinifex/spinifex/handlers/ec2/key"
	"github.com/nats-io/nats.go"
)

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

func ImportKeyPair(ctx context.Context, input *ec2.ImportKeyPairInput, natsConn *nats.Conn, accountID string) (output ec2.ImportKeyPairOutput, err error) {
	err = ValidateImportKeyPairInput(input)

	if err != nil {
		return output, err
	}

	keyService := handlers_ec2_key.NewNATSKeyService(natsConn)
	result, err := keyService.ImportKeyPair(ctx, input, accountID)

	if err != nil {
		return output, err
	}

	output = *result
	return output, nil
}
