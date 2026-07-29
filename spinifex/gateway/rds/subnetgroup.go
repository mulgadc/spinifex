package gateway_rds

import (
	"context"

	"github.com/aws/aws-sdk-go/service/rds"
	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
	"github.com/nats-io/nats.go"
)

// The nested Subnets list is rendered by the shared XML marshaller off the SDK's
// own struct tags, so nothing here shapes the response.
func CreateDBSubnetGroup(ctx context.Context, input *rds.CreateDBSubnetGroupInput, nc *nats.Conn, caller Caller) (any, error) {
	return handlers_rds.NewNATSService(nc).CreateDBSubnetGroup(ctx, input, caller.AccountID)
}

func DescribeDBSubnetGroups(ctx context.Context, input *rds.DescribeDBSubnetGroupsInput, nc *nats.Conn, caller Caller) (any, error) {
	return handlers_rds.NewNATSService(nc).DescribeDBSubnetGroups(ctx, input, caller.AccountID)
}

func DeleteDBSubnetGroup(ctx context.Context, input *rds.DeleteDBSubnetGroupInput, nc *nats.Conn, caller Caller) (any, error) {
	return handlers_rds.NewNATSService(nc).DeleteDBSubnetGroup(ctx, input, caller.AccountID)
}
