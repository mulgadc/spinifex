// Package snapshot implements the EC2 EBS snapshot actions: the
// SnapshotService contract, its daemon-side implementation over the object
// store and JetStream KV, and its NATS client.
package snapshot

import (
	"context"

	"github.com/aws/aws-sdk-go/service/ec2"
)

// SnapshotService defines the interface for EC2 snapshot operations.
type SnapshotService interface {
	CreateSnapshot(ctx context.Context, input *ec2.CreateSnapshotInput, accountID string) (*ec2.Snapshot, error)
	DescribeSnapshots(ctx context.Context, input *ec2.DescribeSnapshotsInput, accountID string) (*ec2.DescribeSnapshotsOutput, error)
	DeleteSnapshot(ctx context.Context, input *ec2.DeleteSnapshotInput, accountID string) (*ec2.DeleteSnapshotOutput, error)
	CopySnapshot(ctx context.Context, input *ec2.CopySnapshotInput, accountID string) (*ec2.CopySnapshotOutput, error)
}
