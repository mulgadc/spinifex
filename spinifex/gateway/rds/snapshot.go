package gateway_rds

import (
	"context"

	"github.com/aws/aws-sdk-go/service/rds"
	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
	"github.com/nats-io/nats.go"
)

// DB snapshots are addressed by their own identifier within the caller's
// account, so nothing here needs the EC2 snapshot the record points at — that
// stays system-owned and unreachable from the customer's EC2 surface.
func CreateDBSnapshot(ctx context.Context, input *rds.CreateDBSnapshotInput, nc *nats.Conn, caller Caller) (any, error) {
	return handlers_rds.NewNATSService(nc).CreateDBSnapshot(ctx, input, caller.AccountID)
}

func DescribeDBSnapshots(ctx context.Context, input *rds.DescribeDBSnapshotsInput, nc *nats.Conn, caller Caller) (any, error) {
	return handlers_rds.NewNATSService(nc).DescribeDBSnapshots(ctx, input, caller.AccountID)
}

func DeleteDBSnapshot(ctx context.Context, input *rds.DeleteDBSnapshotInput, nc *nats.Conn, caller Caller) (any, error) {
	return handlers_rds.NewNATSService(nc).DeleteDBSnapshot(ctx, input, caller.AccountID)
}

func RestoreDBInstanceFromDBSnapshot(ctx context.Context, input *rds.RestoreDBInstanceFromDBSnapshotInput, nc *nats.Conn, caller Caller) (any, error) {
	return handlers_rds.NewNATSService(nc).RestoreDBInstanceFromDBSnapshot(ctx, input, caller.AccountID)
}

// One DBInstanceAutomatedBackup per instance with automated backups. The
// individual snapshots stay listable through DescribeDBSnapshots with
// --snapshot-type automated, which is where AWS puts them too.
func DescribeDBInstanceAutomatedBackups(ctx context.Context, input *rds.DescribeDBInstanceAutomatedBackupsInput, nc *nats.Conn, caller Caller) (any, error) {
	return handlers_rds.NewNATSService(nc).DescribeDBInstanceAutomatedBackups(ctx, input, caller.AccountID)
}
