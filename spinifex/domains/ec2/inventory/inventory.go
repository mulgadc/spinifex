// Package inventory is the EC2-owned read capability for an account's complete
// instance list, for callers outside the EC2 domain.
package inventory

import (
	"context"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2instanceapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/instance"
	"github.com/nats-io/nats.go"
)

// Instances lists every non-terminated instance an account owns, or returns an
// error. It never returns a shorter list than the truth.
type Instances interface {
	ListAccountInstances(ctx context.Context, accountID string) (*ec2.DescribeInstancesOutput, error)
}

// NATSInstances reads the complete list across nodeIDs over NATS.
type NATSInstances struct {
	nc      *nats.Conn
	nodeIDs []string
}

var _ Instances = (*NATSInstances)(nil)

// NewNATSInstances returns a reader over the configured node set.
func NewNATSInstances(nc *nats.Conn, nodeIDs []string) *NATSInstances {
	return &NATSInstances{nc: nc, nodeIDs: nodeIDs}
}

// ListAccountInstances passes through the strict listing unchanged: any node
// that did not answer, or any record that could not be read, is an error.
func (n *NATSInstances) ListAccountInstances(ctx context.Context, accountID string) (*ec2.DescribeInstancesOutput, error) {
	return ec2instanceapi.DescribeAccountInstancesStrict(ctx, n.nc, n.nodeIDs, accountID)
}
