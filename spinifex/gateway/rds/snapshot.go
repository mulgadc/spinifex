package gateway_rds

import (
	"context"

	"github.com/aws/aws-sdk-go/service/rds"
	rdsinstance "github.com/mulgadc/spinifex/spinifex/domains/rds/instance"
	"github.com/nats-io/nats.go"
)

// CreateDBSnapshot addresses DB snapshots by their own identifier within the caller's account, so
// nothing here needs the EC2 snapshot the record points at; that stays system-owned and
// unreachable from the customer's EC2 surface.
func CreateDBSnapshot(ctx context.Context, input *rds.CreateDBSnapshotInput, nc *nats.Conn, caller Caller) (any, error) {
	return rdsinstance.NewNATSService(nc).CreateDBSnapshot(ctx, input, caller.AccountID)
}

// DescribeDBSnapshots implements the RDS DescribeDBSnapshots action, covering manual and
// automated snapshots in the caller's account.
func DescribeDBSnapshots(ctx context.Context, input *rds.DescribeDBSnapshotsInput, nc *nats.Conn, caller Caller) (any, error) {
	return rdsinstance.NewNATSService(nc).DescribeDBSnapshots(ctx, input, caller.AccountID)
}

// DeleteDBSnapshot implements the RDS DeleteDBSnapshot action for the caller's account.
func DeleteDBSnapshot(ctx context.Context, input *rds.DeleteDBSnapshotInput, nc *nats.Conn, caller Caller) (any, error) {
	return rdsinstance.NewNATSService(nc).DeleteDBSnapshot(ctx, input, caller.AccountID)
}

// RestoreDBInstanceFromDBSnapshot implements the RDS action of the same name, creating a new DB
// instance from a snapshot in the caller's account.
func RestoreDBInstanceFromDBSnapshot(ctx context.Context, input *rds.RestoreDBInstanceFromDBSnapshotInput, nc *nats.Conn, caller Caller) (any, error) {
	return rdsinstance.NewNATSService(nc).RestoreDBInstanceFromDBSnapshot(ctx, input, caller.AccountID)
}

// DescribeDBInstanceAutomatedBackups returns one DBInstanceAutomatedBackup per instance with
// automated backups. The snapshots themselves stay listable through DescribeDBSnapshots with
// --snapshot-type automated, which is where AWS puts them too.
func DescribeDBInstanceAutomatedBackups(ctx context.Context, input *rds.DescribeDBInstanceAutomatedBackupsInput, nc *nats.Conn, caller Caller) (any, error) {
	return rdsinstance.NewNATSService(nc).DescribeDBInstanceAutomatedBackups(ctx, input, caller.AccountID)
}
