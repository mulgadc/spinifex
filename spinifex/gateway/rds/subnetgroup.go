package gateway_rds

import (
	"context"

	"github.com/aws/aws-sdk-go/service/rds"
	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
	"github.com/nats-io/nats.go"
)

// CreateDBSubnetGroup implements the RDS action. The nested Subnets list is rendered by the
// shared XML marshaller off the SDK's own struct tags, so nothing here shapes the response.
func CreateDBSubnetGroup(ctx context.Context, input *rds.CreateDBSubnetGroupInput, nc *nats.Conn, caller Caller) (any, error) {
	return handlers_rds.NewNATSService(nc).CreateDBSubnetGroup(ctx, input, caller.AccountID)
}

// DescribeDBSubnetGroups implements the RDS DescribeDBSubnetGroups action for the caller's
// account.
func DescribeDBSubnetGroups(ctx context.Context, input *rds.DescribeDBSubnetGroupsInput, nc *nats.Conn, caller Caller) (any, error) {
	return handlers_rds.NewNATSService(nc).DescribeDBSubnetGroups(ctx, input, caller.AccountID)
}

// ModifyDBSubnetGroup implements the RDS ModifyDBSubnetGroup action, replacing a subnet group's
// subnets or description.
func ModifyDBSubnetGroup(ctx context.Context, input *rds.ModifyDBSubnetGroupInput, nc *nats.Conn, caller Caller) (any, error) {
	return handlers_rds.NewNATSService(nc).ModifyDBSubnetGroup(ctx, input, caller.AccountID)
}

// DeleteDBSubnetGroup implements the RDS DeleteDBSubnetGroup action for the caller's account.
func DeleteDBSubnetGroup(ctx context.Context, input *rds.DeleteDBSubnetGroupInput, nc *nats.Conn, caller Caller) (any, error) {
	return handlers_rds.NewNATSService(nc).DeleteDBSubnetGroup(ctx, input, caller.AccountID)
}
