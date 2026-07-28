package gateway_rds

import (
	"context"

	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/nats-io/nats.go"
)

// DescribeDBInstances returns the caller's DB instances. Until CreateDBInstance
// lands the result set is always empty, which is the honest answer for an
// account that has none and makes the request/response contract exercisable.
func DescribeDBInstances(_ context.Context, _ *rds.DescribeDBInstancesInput, _ *nats.Conn, _ Caller) (any, error) {
	return &rds.DescribeDBInstancesOutput{DBInstances: []*rds.DBInstance{}}, nil
}
