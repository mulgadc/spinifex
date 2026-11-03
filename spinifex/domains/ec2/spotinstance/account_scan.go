package spotinstance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
)

// AccountSpotRequest is one persisted request in an account scan.
type AccountSpotRequest struct {
	RequestID string
	State     string
}

// AccountScan lists every request an account holds in both buckets.
type AccountScan struct {
	Active   []AccountSpotRequest
	Terminal []AccountSpotRequest
}

// ScanAccount lists the account's requests, or fails. Unlike
// DescribeSpotInstanceRequests it never skips a record it cannot read: a
// shorter list would be taken as proof that nothing is left.
func (s *SpotInstanceServiceImpl) ScanAccount(ctx context.Context, accountID string) (*AccountScan, error) {
	if accountID == "" {
		return nil, errors.New("spot request scan needs an account ID")
	}
	active, err := scanBucket(ctx, s.activeKV, accountID)
	if err != nil {
		return nil, fmt.Errorf("scan active spot requests: %w", err)
	}
	terminal, err := scanBucket(ctx, s.terminalKV, accountID)
	if err != nil {
		return nil, fmt.Errorf("scan terminal spot requests: %w", err)
	}
	return &AccountScan{Active: active, Terminal: terminal}, nil
}

func scanBucket(ctx context.Context, kv jetstream.KeyValue, accountID string) ([]AccountSpotRequest, error) {
	keys, err := kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, err
	}
	prefix := accountID + "."
	var out []AccountSpotRequest
	for _, key := range keys {
		if key == kvutil.VersionKey || !strings.HasPrefix(key, prefix) {
			continue
		}
		entry, err := kv.Get(ctx, key)
		if err != nil {
			// Expired from the terminal bucket between listing and read.
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				continue
			}
			return nil, fmt.Errorf("read %s: %w", key, err)
		}
		var record SpotRequestRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			return nil, fmt.Errorf("decode %s: %w", key, err)
		}
		if record.Request == nil || aws.StringValue(record.Request.SpotInstanceRequestId) == "" {
			return nil, fmt.Errorf("record %s carries no request", key)
		}
		out = append(out, AccountSpotRequest{
			RequestID: aws.StringValue(record.Request.SpotInstanceRequestId),
			State:     aws.StringValue(record.Request.State),
		})
	}
	return out, nil
}
