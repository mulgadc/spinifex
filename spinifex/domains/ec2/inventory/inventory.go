// Package inventory is the EC2-owned read capability for an account's complete
// instance list, for callers outside the EC2 domain.
package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2instanceapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/instance"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
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

// NewENITagReader returns a reader of one ENI's tags straight from its stored
// record, so an ENI that is absent and one that cannot be read are told apart:
// a missing key is found=false, a failed read or an undecodable record is an
// error. accountID is the account the ENI is stored under.
func NewENITagReader(js jetstream.JetStream, accountID string) func(ctx context.Context, eniID string) (map[string]string, bool, error) {
	return func(ctx context.Context, eniID string) (map[string]string, bool, error) {
		kv, err := js.KeyValue(ctx, ec2vpc.KVBucketENIs)
		if err != nil {
			return nil, false, fmt.Errorf("open ENI bucket: %w", err)
		}
		entry, err := kv.Get(ctx, kvstore.AccountKey(accountID, eniID))
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, fmt.Errorf("read ENI record %s: %w", eniID, err)
		}
		var record ec2vpc.ENIRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			return nil, false, fmt.Errorf("decode ENI record %s: %w", eniID, err)
		}
		return record.Tags, true, nil
	}
}
