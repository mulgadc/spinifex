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

// ValidateDeleteKeyPairInput returns MissingParameter for a nil input or when neither KeyName nor
// KeyPairId is set.
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

// DeleteKeyPair implements the EC2 DeleteKeyPair action, deleting the key pair named by KeyName
// or KeyPairId in accountID via the NATS key service.
func DeleteKeyPair(ctx context.Context, input *ec2.DeleteKeyPairInput, natsConn *nats.Conn, accountID string) (output ec2.DeleteKeyPairOutput, err error) {
	err = ValidateDeleteKeyPairInput(input)

	if err != nil {
		return output, err
	}

	keyService := ec2key.NewNATSKeyService(natsConn)
	result, err := keyService.DeleteKeyPair(ctx, input, accountID)

	if err != nil {
		slog.ErrorContext(ctx, "DeleteKeyPair failed", "err", err)
		return output, err
	}

	output = *result
	return output, nil
}
