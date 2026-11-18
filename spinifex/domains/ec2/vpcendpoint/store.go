package vpcendpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/migrate"
	"github.com/nats-io/nats.go/jetstream"
)

// errConflict reports a revision-checked write that lost its race.
var errConflict = errors.New("vpcendpoint: revision conflict")

// TableLookup is the narrow route-table query endpoints need, installed by
// the composition root so this package never imports the route tables.
type TableLookup interface {
	// RouteTableVpc returns the VPC of accountID's table rtbID, or "" when the
	// table does not exist.
	RouteTableVpc(ctx context.Context, accountID, rtbID string) (string, error)
}

// Store holds the endpoint bucket and the VPC bucket creates validate against.
type Store struct {
	kv          jetstream.KeyValue
	vpcKV       jetstream.KeyValue
	tables      TableLookup
	centralTags CentralTagStore
	now         func() time.Time
}

// NewStore wraps already-open buckets.
func NewStore(kv, vpcKV jetstream.KeyValue) *Store {
	return &Store{kv: kv, vpcKV: vpcKV, now: time.Now}
}

// OpenStore opens or creates the endpoint bucket and opens the VPC bucket.
func OpenStore(ctx context.Context, js jetstream.JetStream) (*Store, error) {
	kv, err := kvstore.GetOrCreateBucket(ctx, js, KVBucketVpcEndpoints, 5)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", KVBucketVpcEndpoints, err)
	}
	if err := migrate.DefaultRegistry.RunKV(ctx, KVBucketVpcEndpoints, kv, KVBucketVpcEndpointsVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketVpcEndpoints, err)
	}
	vpcKV, err := kvstore.GetOrCreateBucket(ctx, js, ec2vpc.KVBucketVPCs, 10)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", ec2vpc.KVBucketVPCs, err)
	}
	return NewStore(kv, vpcKV), nil
}

// SetTableLookup installs the route-table query. Called by the daemon once
// the route-table service exists.
func (s *Store) SetTableLookup(t TableLookup) { s.tables = t }

func getJSON[T any](ctx context.Context, kv jetstream.KeyValue, key string) (*T, uint64, error) {
	entry, err := kv.Get(ctx, key)
	if err != nil {
		// A key KV cannot hold names nothing: a malformed ID is absent.
		if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrInvalidKey) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("read %s: %w", key, err)
	}
	var v T
	if err := json.Unmarshal(entry.Value(), &v); err != nil {
		return nil, 0, fmt.Errorf("decode %s: %w", key, err)
	}
	return &v, entry.Revision(), nil
}

func putJSON(ctx context.Context, kv jetstream.KeyValue, key string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %s: %w", key, err)
	}
	if _, err := kv.Put(ctx, key, data); err != nil {
		return fmt.Errorf("write %s: %w", key, err)
	}
	return nil
}

// writeJSON creates key when rev is 0, else updates it at rev; a lost race is
// errConflict.
func writeJSON(ctx context.Context, kv jetstream.KeyValue, key string, v any, rev uint64) (uint64, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return 0, fmt.Errorf("encode %s: %w", key, err)
	}
	if rev == 0 {
		rev, err = kv.Create(ctx, key, data)
	} else {
		rev, err = kv.Update(ctx, key, data, rev)
	}
	if kvstore.IsRevisionConflict(err) {
		return 0, errConflict
	}
	if err != nil {
		return 0, fmt.Errorf("write %s: %w", key, err)
	}
	return rev, nil
}

// deleteAt removes key at rev; an absent key is already deleted.
func deleteAt(ctx context.Context, kv jetstream.KeyValue, key string, rev uint64) error {
	err := kv.Delete(ctx, key, jetstream.LastRevision(rev))
	switch {
	case err == nil, errors.Is(err, jetstream.ErrKeyNotFound):
		return nil
	case kvstore.IsRevisionConflict(err):
		return errConflict
	}
	return fmt.Errorf("delete %s: %w", key, err)
}

func deleteKey(ctx context.Context, kv jetstream.KeyValue, key string) error {
	if err := kv.Delete(ctx, key); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return fmt.Errorf("delete %s: %w", key, err)
	}
	return nil
}

// listKeys returns the keys under prefix, which ends in ".".
func listKeys(ctx context.Context, kv jetstream.KeyValue, prefix string) ([]string, error) {
	lister, err := kv.ListKeysFiltered(ctx, prefix+"*")
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("list %s: %w", prefix, err)
	}
	var keys []string
	for key := range lister.Keys() {
		keys = append(keys, key)
	}
	// Cancellation closes the lister early, and a short list must not read as complete.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return keys, nil
}

func (s *Store) load(ctx context.Context, accountID, id string) (*EndpointRecord, uint64, error) {
	return getJSON[EndpointRecord](ctx, s.kv, recordKey(accountID, id))
}

// live reports whether rec counts against limits and dependencies: available,
// or pending young enough to be a create still in flight.
func (s *Store) live(rec *EndpointRecord) bool {
	switch rec.State {
	case StateAvailable:
		return true
	case StatePending:
		return s.now().Sub(rec.CreatedAt) < claimGrace
	}
	return false
}

// vpcRecord reads accountID's VPC; nil when absent.
func (s *Store) vpcRecord(ctx context.Context, accountID, vpcID string) (*ec2vpc.VPCRecord, error) {
	rec, _, err := getJSON[ec2vpc.VPCRecord](ctx, s.vpcKV, kvstore.AccountKey(accountID, vpcID))
	return rec, err
}

// accountRecords returns accountID's stored records in every state.
func (s *Store) accountRecords(ctx context.Context, accountID string) ([]*EndpointRecord, error) {
	keys, err := listKeys(ctx, s.kv, accountID+".")
	if err != nil {
		return nil, err
	}
	var out []*EndpointRecord
	for _, key := range keys {
		rec, _, err := getJSON[EndpointRecord](ctx, s.kv, key)
		if err != nil {
			return nil, err
		}
		if rec != nil {
			out = append(out, rec)
		}
	}
	return out, nil
}

// VpcBlocksDeletion reports whether an endpoint blocks deleting accountID's
// vpcID. An endpoint's VPC is its owner's, so only the owner's records count.
func (s *Store) VpcBlocksDeletion(ctx context.Context, accountID, vpcID string) (bool, error) {
	recs, err := s.accountRecords(ctx, accountID)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(recs, func(r *EndpointRecord) bool { return r.VpcID == vpcID && s.live(r) }), nil
}

// claimHolder returns the endpoint rtbID's claim names, and that endpoint's
// record if it still lists the table and is available. Either may be nil.
func (s *Store) claimHolder(ctx context.Context, rtbID string) (*claim, *EndpointRecord, error) {
	c, _, err := getJSON[claim](ctx, s.kv, claimKey(rtbID))
	if err != nil || c == nil {
		return nil, nil, err
	}
	rec, _, err := s.load(ctx, c.AccountID, c.VpceID)
	if err != nil {
		return nil, nil, err
	}
	if rec == nil || rec.State != StateAvailable || !rec.lists(rtbID) {
		return c, nil, nil
	}
	return c, rec, nil
}

// EffectiveEndpoints returns, for each of rtbIDs that carries an endpoint
// route, the endpoint it routes to. The caller vouches that the tables exist.
func (s *Store) EffectiveEndpoints(ctx context.Context, rtbIDs []string) (map[string]string, error) {
	out := map[string]string{}
	for _, t := range rtbIDs {
		_, rec, err := s.claimHolder(ctx, t)
		if err != nil {
			return nil, err
		}
		if rec != nil {
			out[t] = rec.ID
		}
	}
	return out, nil
}

// effectiveTables returns the listed tables whose association with rec is
// effective: rec is available, the claim names it, and the table exists in
// rec's VPC.
func (s *Store) effectiveTables(ctx context.Context, rec *EndpointRecord) ([]string, error) {
	if rec.State != StateAvailable {
		return nil, nil
	}
	out := []string{}
	for _, t := range rec.RouteTableIds {
		c, _, err := getJSON[claim](ctx, s.kv, claimKey(t))
		if err != nil {
			return nil, err
		}
		if c == nil || c.VpceID != rec.ID {
			continue
		}
		vpc, err := s.tableVpc(ctx, rec.AccountID, t)
		if err != nil {
			return nil, err
		}
		if vpc == rec.VpcID {
			out = append(out, t)
		}
	}
	return out, nil
}

func (s *Store) tableVpc(ctx context.Context, accountID, rtbID string) (string, error) {
	if s.tables == nil {
		return "", errors.New("vpcendpoint: no route-table lookup installed")
	}
	return s.tables.RouteTableVpc(ctx, accountID, rtbID)
}
