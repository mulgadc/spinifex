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

// ValidateCreateSnapshotInput validates the input parameters for CreateSnapshot.
func ValidateCreateSnapshotInput(input *ec2.CreateSnapshotInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	if input.VolumeId == nil || *input.VolumeId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}

	if !strings.HasPrefix(*input.VolumeId, "vol-") {
		return errors.New(awserrors.ErrorInvalidVolumeIDMalformed)
	}

	return nil
}

// CreateSnapshot handles the EC2 CreateSnapshot API call.
func CreateSnapshot(ctx context.Context, input *ec2.CreateSnapshotInput, natsConn *nats.Conn, accountID string) (ec2.Snapshot, error) {
	var output ec2.Snapshot

	if err := ValidateCreateSnapshotInput(input); err != nil {
		return output, err
	}

	svc := ec2snapshot.NewNATSSnapshotService(natsConn)
	result, err := svc.CreateSnapshot(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
