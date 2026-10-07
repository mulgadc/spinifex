package key

import (
	"context"
	"errors"
	"log/slog"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2key "github.com/mulgadc/spinifex/spinifex/domains/ec2/key"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// ValidateCreateKeyPairInput returns MissingParameter for a nil input or an empty KeyName.
func ValidateCreateKeyPairInput(input *ec2.CreateKeyPairInput) (err error) {
	if input == nil {
		return errors.New(awserrors.ErrorMissingParameter)
	}

	if input.KeyName == nil || *input.KeyName == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}

	return err
}

// CreateKeyPair implements the EC2 CreateKeyPair action. The NATS key service generates the key
// pair for accountID and the output carries the private key material.
func CreateKeyPair(ctx context.Context, input *ec2.CreateKeyPairInput, natsConn *nats.Conn, accountID string) (output ec2.CreateKeyPairOutput, err error) {
	err = ValidateCreateKeyPairInput(input)

	if err != nil {
		return output, err
	}

	keyService := ec2key.NewNATSKeyService(natsConn)
	result, err := keyService.CreateKeyPair(ctx, input, accountID)

	if err != nil {
		slog.ErrorContext(ctx, "CreateKeyPair failed", "err", err)
		return output, err
	}

	output = *result
	return output, nil
}
