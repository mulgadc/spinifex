package gateway_rds

import (
	"context"

	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/nats-io/nats.go"
)

// DescribeDBInstances returns the caller's DB instances.
//
// Until the KV-backed instance record lands with CreateDBInstance the result set
// is always empty. The action is live regardless because an empty list is the
// honest answer for an account that has no DB instances, and it makes the whole
// contract — Query request in, IAM-style XML envelope out — exercisable end to
// end before any handler body exists.
func DescribeDBInstances(_ context.Context, _ *rds.DescribeDBInstancesInput, _ *nats.Conn, _ string) (any, error) {
	return &rds.DescribeDBInstancesOutput{DBInstances: []*rds.DBInstance{}}, nil
}
