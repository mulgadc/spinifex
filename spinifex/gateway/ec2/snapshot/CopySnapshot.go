package gateway_ec2_snapshot

import (
	"context"
	"errors"
	"strings"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2snapshot "github.com/mulgadc/spinifex/spinifex/domains/ec2/snapshot"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// ValidateCopySnapshotInput validates the input parameters for CopySnapshot.
func ValidateCopySnapshotInput(input *ec2.CopySnapshotInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	if input.SourceSnapshotId == nil || *input.SourceSnapshotId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}

	if !strings.HasPrefix(*input.SourceSnapshotId, "snap-") {
		return errors.New(awserrors.ErrorInvalidSnapshotIDMalformed)
	}

	if input.SourceRegion == nil || *input.SourceRegion == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}

	return nil
}

// CopySnapshot handles the EC2 CopySnapshot API call.
func CopySnapshot(ctx context.Context, input *ec2.CopySnapshotInput, natsConn *nats.Conn, accountID string) (ec2.CopySnapshotOutput, error) {
	var output ec2.CopySnapshotOutput

	if err := ValidateCopySnapshotInput(input); err != nil {
		return output, err
	}

	svc := ec2snapshot.NewNATSSnapshotService(natsConn)
	result, err := svc.CopySnapshot(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
