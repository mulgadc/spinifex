package snapshot

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// DescribeSnapshotAttribute reads the snapshot through DescribeSnapshots. A
// snapshot cannot be shared or carry a product code, so createVolumePermission
// and productCodes are both empty.
func DescribeSnapshotAttribute(ctx context.Context, input *ec2.DescribeSnapshotAttributeInput, natsConn *nats.Conn, accountID string) (ec2.DescribeSnapshotAttributeOutput, error) {
	var output ec2.DescribeSnapshotAttributeOutput
	if input == nil || aws.StringValue(input.SnapshotId) == "" || aws.StringValue(input.Attribute) == "" {
		return output, errors.New(awserrors.ErrorMissingParameter)
	}
	attribute := aws.StringValue(input.Attribute)
	if attribute != ec2.SnapshotAttributeNameCreateVolumePermission && attribute != ec2.SnapshotAttributeNameProductCodes {
		return output, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	snapshots, err := DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{SnapshotIds: []*string{input.SnapshotId}}, natsConn, accountID)
	if err != nil {
		return output, err
	}
	if len(snapshots.Snapshots) == 0 {
		return output, errors.New(awserrors.ErrorInvalidSnapshotNotFound)
	}

	output.SnapshotId = snapshots.Snapshots[0].SnapshotId
	if attribute == ec2.SnapshotAttributeNameCreateVolumePermission {
		output.CreateVolumePermissions = []*ec2.CreateVolumePermission{}
	} else {
		output.ProductCodes = []*ec2.ProductCode{}
	}
	return output, nil
}
