package gateway_ec2_key

import (
	"context"
	"errors"
	"log/slog"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_key "github.com/mulgadc/spinifex/spinifex/handlers/ec2/key"
	"github.com/nats-io/nats.go"
)

func ValidateCreateKeyPairInput(input *ec2.CreateKeyPairInput) (err error) {
	if input == nil {
		return errors.New(awserrors.ErrorMissingParameter)
	}

	if input.KeyName == nil || *input.KeyName == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}

	return err
}

func CreateKeyPair(ctx context.Context, input *ec2.CreateKeyPairInput, natsConn *nats.Conn, accountID string) (output ec2.CreateKeyPairOutput, err error) {
	err = ValidateCreateKeyPairInput(input)

	if err != nil {
		return output, err
	}

	keyService := handlers_ec2_key.NewNATSKeyService(natsConn)
	result, err := keyService.CreateKeyPair(ctx, input, accountID)

	if err != nil {
		slog.ErrorContext(ctx, "CreateKeyPair failed", "err", err)
		return output, err
	}

	output = *result
	return output, nil
}
