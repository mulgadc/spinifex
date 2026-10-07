package gateway_rds

import (
	"context"

	"github.com/aws/aws-sdk-go/service/rds"
	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
	"github.com/nats-io/nats.go"
)

// CreateDBInstance enforces the DB-instance quota, then hands off: the whole orchestration runs
// on the daemon side, so this only carries the caller's account through, and the request body
// never names the owner.
func CreateDBInstance(ctx context.Context, input *rds.CreateDBInstanceInput, nc *nats.Conn, caller Caller, env Env) (any, error) {
	if env.QuotaCheck != nil {
		if err := env.QuotaCheck(ctx, caller.AccountID, 1); err != nil {
			return nil, err
		}
	}
	return handlers_rds.NewNATSService(nc).CreateDBInstance(ctx, input, caller.AccountID)
}

// DescribeDBInstances implements the RDS DescribeDBInstances action for the caller's account.
func DescribeDBInstances(ctx context.Context, input *rds.DescribeDBInstancesInput, nc *nats.Conn, caller Caller) (any, error) {
	return handlers_rds.NewNATSService(nc).DescribeDBInstances(ctx, input, caller.AccountID)
}

// RebootDBInstance implements the RDS RebootDBInstance action for the caller's account.
func RebootDBInstance(ctx context.Context, input *rds.RebootDBInstanceInput, nc *nats.Conn, caller Caller) (any, error) {
	return handlers_rds.NewNATSService(nc).RebootDBInstance(ctx, input, caller.AccountID)
}

// StartDBInstance implements the RDS StartDBInstance action, starting a stopped instance in the
// caller's account.
func StartDBInstance(ctx context.Context, input *rds.StartDBInstanceInput, nc *nats.Conn, caller Caller) (any, error) {
	return handlers_rds.NewNATSService(nc).StartDBInstance(ctx, input, caller.AccountID)
}

// StopDBInstance implements the RDS StopDBInstance action for the caller's account.
func StopDBInstance(ctx context.Context, input *rds.StopDBInstanceInput, nc *nats.Conn, caller Caller) (any, error) {
	return handlers_rds.NewNATSService(nc).StopDBInstance(ctx, input, caller.AccountID)
}

// ModifyDBInstance implements the RDS ModifyDBInstance action for the caller's account.
func ModifyDBInstance(ctx context.Context, input *rds.ModifyDBInstanceInput, nc *nats.Conn, caller Caller) (any, error) {
	return handlers_rds.NewNATSService(nc).ModifyDBInstance(ctx, input, caller.AccountID)
}

// DeleteDBInstance implements the RDS DeleteDBInstance action for the caller's account.
func DeleteDBInstance(ctx context.Context, input *rds.DeleteDBInstanceInput, nc *nats.Conn, caller Caller) (any, error) {
	return handlers_rds.NewNATSService(nc).DeleteDBInstance(ctx, input, caller.AccountID)
}

// DescribeEvents is scoped to the caller's account: an event ring is per-tenant, so there is no
// cross-account read even for a resource that has since been deleted.
func DescribeEvents(ctx context.Context, input *rds.DescribeEventsInput, nc *nats.Conn, caller Caller) (any, error) {
	return handlers_rds.NewNATSService(nc).DescribeEvents(ctx, input, caller.AccountID)
}
