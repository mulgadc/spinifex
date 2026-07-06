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

func ValidateDeleteKeyPairInput(input *ec2.DeleteKeyPairInput) (err error) {
	if input == nil {
		return errors.New(awserrors.ErrorMissingParameter)
	}

	// At least one of KeyName or KeyPairId must be provided
	if (input.KeyName == nil || *input.KeyName == "") && (input.KeyPairId == nil || *input.KeyPairId == "") {
		return errors.New(awserrors.ErrorMissingParameter)
	}

	return err
}

func DeleteKeyPair(ctx context.Context, input *ec2.DeleteKeyPairInput, natsConn *nats.Conn, accountID string) (output ec2.DeleteKeyPairOutput, err error) {
	err = ValidateDeleteKeyPairInput(input)

	if err != nil {
		return output, err
	}

	keyService := handlers_ec2_key.NewNATSKeyService(natsConn)
	result, err := keyService.DeleteKeyPair(ctx, input, accountID)

	if err != nil {
		slog.ErrorContext(ctx, "DeleteKeyPair failed", "err", err)
		return output, err
	}

	output = *result
	return output, nil
}
