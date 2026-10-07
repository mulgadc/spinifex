package snapshot

import (
	"context"
	"errors"
	"strings"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2snapshot "github.com/mulgadc/spinifex/spinifex/domains/ec2/snapshot"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// ValidateDeleteSnapshotInput validates the input parameters for DeleteSnapshot.
func ValidateDeleteSnapshotInput(input *ec2.DeleteSnapshotInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	if input.SnapshotId == nil || *input.SnapshotId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}

	if !strings.HasPrefix(*input.SnapshotId, "snap-") {
		return errors.New(awserrors.ErrorInvalidSnapshotIDMalformed)
	}

	return nil
}

// DeleteSnapshot handles the EC2 DeleteSnapshot API call.
func DeleteSnapshot(ctx context.Context, input *ec2.DeleteSnapshotInput, natsConn *nats.Conn, accountID string) (ec2.DeleteSnapshotOutput, error) {
	var output ec2.DeleteSnapshotOutput

	if err := ValidateDeleteSnapshotInput(input); err != nil {
		return output, err
	}

	svc := ec2snapshot.NewNATSSnapshotService(natsConn)
	result, err := svc.DeleteSnapshot(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
