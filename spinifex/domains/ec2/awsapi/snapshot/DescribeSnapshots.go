// Package snapshot implements the EC2 EBS snapshot actions: it
// validates each request and forwards it to the snapshot service over NATS.
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

// ValidateDescribeSnapshotsInput validates the input parameters for DescribeSnapshots.
func ValidateDescribeSnapshotsInput(input *ec2.DescribeSnapshotsInput) error {
	if input == nil {
		return nil
	}

	for _, id := range input.SnapshotIds {
		if id != nil && *id != "" && !strings.HasPrefix(*id, "snap-") {
			return errors.New(awserrors.ErrorInvalidSnapshotIDMalformed)
		}
	}

	return nil
}

// DescribeSnapshots handles the EC2 DescribeSnapshots API call.
func DescribeSnapshots(ctx context.Context, input *ec2.DescribeSnapshotsInput, natsConn *nats.Conn, accountID string) (ec2.DescribeSnapshotsOutput, error) {
	var output ec2.DescribeSnapshotsOutput

	if err := ValidateDescribeSnapshotsInput(input); err != nil {
		return output, err
	}

	svc := ec2snapshot.NewNATSSnapshotService(natsConn)
	result, err := svc.DescribeSnapshots(ctx, input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
