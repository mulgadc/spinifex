package handlers_ec2_snapshot

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

var _ SnapshotService = (*NATSSnapshotService)(nil)

// NATSSnapshotService handles snapshot operations via NATS messaging
type NATSSnapshotService struct {
	natsConn *nats.Conn
}

// NewNATSSnapshotService creates a new NATS-based snapshot service
func NewNATSSnapshotService(conn *nats.Conn) SnapshotService {
	return &NATSSnapshotService{natsConn: conn}
}

func (s *NATSSnapshotService) CreateSnapshot(ctx context.Context, input *ec2.CreateSnapshotInput, accountID string) (*ec2.Snapshot, error) {
	return utils.NATSRequest[ec2.Snapshot](ctx, s.natsConn, "ec2.CreateSnapshot", input, 120*time.Second, accountID)
}

func (s *NATSSnapshotService) DescribeSnapshots(ctx context.Context, input *ec2.DescribeSnapshotsInput, accountID string) (*ec2.DescribeSnapshotsOutput, error) {
	return utils.NATSRequest[ec2.DescribeSnapshotsOutput](ctx, s.natsConn, "ec2.DescribeSnapshots", input, 30*time.Second, accountID)
}

func (s *NATSSnapshotService) DeleteSnapshot(ctx context.Context, input *ec2.DeleteSnapshotInput, accountID string) (*ec2.DeleteSnapshotOutput, error) {
	return utils.NATSRequest[ec2.DeleteSnapshotOutput](ctx, s.natsConn, "ec2.DeleteSnapshot", input, 60*time.Second, accountID)
}

func (s *NATSSnapshotService) CopySnapshot(ctx context.Context, input *ec2.CopySnapshotInput, accountID string) (*ec2.CopySnapshotOutput, error) {
	return utils.NATSRequest[ec2.CopySnapshotOutput](ctx, s.natsConn, "ec2.CopySnapshot", input, 120*time.Second, accountID)
}
