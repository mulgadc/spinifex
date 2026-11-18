package vpcendpoint

import (
	"context"
	"errors"
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
)

// Association is a claim whose endpoint is available and lists the table.
// It is effective once the reader, which holds the tables, finds the table
// in the endpoint's VPC.
type Association struct {
	RouteTableID string
	Endpoint     EndpointRecord
}

// ListAssociations reads every claim in kv and the record it names. It scans
// the bucket, so only node-local loops call it, never a request handler.
func ListAssociations(ctx context.Context, kv jetstream.KeyValue) ([]Association, error) {
	keys, err := kvstore.Keys(ctx, kv)
	if errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", KVBucketVpcEndpoints, err)
	}
	var out []Association
	for _, key := range keys {
		table, ok := rtbFromClaimKey(key)
		if !ok {
			continue
		}
		c, _, err := getJSON[claim](ctx, kv, key)
		if err != nil {
			return nil, err
		}
		if c == nil {
			continue
		}
		rec, _, err := getJSON[EndpointRecord](ctx, kv, recordKey(c.AccountID, c.VpceID))
		if err != nil {
			return nil, err
		}
		if rec == nil || rec.ID != c.VpceID || rec.State != StateAvailable || !rec.lists(table) {
			continue
		}
		out = append(out, Association{RouteTableID: table, Endpoint: *rec})
	}
	return out, nil
}
