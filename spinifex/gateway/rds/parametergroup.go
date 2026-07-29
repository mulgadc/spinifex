package gateway_rds

import (
	"context"

	"github.com/aws/aws-sdk-go/service/rds"
	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
	"github.com/nats-io/nats.go"
)

func CreateDBParameterGroup(ctx context.Context, input *rds.CreateDBParameterGroupInput, nc *nats.Conn, caller Caller) (any, error) {
	return handlers_rds.NewNATSService(nc).CreateDBParameterGroup(ctx, input, caller.AccountID)
}

func DescribeDBParameterGroups(ctx context.Context, input *rds.DescribeDBParameterGroupsInput, nc *nats.Conn, caller Caller) (any, error) {
	return handlers_rds.NewNATSService(nc).DescribeDBParameterGroups(ctx, input, caller.AccountID)
}

// The result is DBParameterGroupNameMessage rather than a group, which is what
// AWS returns and what the Terraform provider reads the applied name from.
func ModifyDBParameterGroup(ctx context.Context, input *rds.ModifyDBParameterGroupInput, nc *nats.Conn, caller Caller) (any, error) {
	return handlers_rds.NewNATSService(nc).ModifyDBParameterGroup(ctx, input, caller.AccountID)
}

func DescribeDBParameters(ctx context.Context, input *rds.DescribeDBParametersInput, nc *nats.Conn, caller Caller) (any, error) {
	return handlers_rds.NewNATSService(nc).DescribeDBParameters(ctx, input, caller.AccountID)
}

func DeleteDBParameterGroup(ctx context.Context, input *rds.DeleteDBParameterGroupInput, nc *nats.Conn, caller Caller) (any, error) {
	return handlers_rds.NewNATSService(nc).DeleteDBParameterGroup(ctx, input, caller.AccountID)
}
