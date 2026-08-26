package handlers_bedrock

import (
	"context"
	"encoding/base64"
	"errors"
	"time"

	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/nats-io/nats.go/jetstream"
)

// KVBucketEndpoints is the single JetStream KV bucket holding every serving
// endpoint's state, across all accounts. Unlike EKS/RDS there is no
// per-account bucket: self-host serving VMs are shared platform infra, not
// per-tenant resources, so one bucket keyed {accountID}/{modelID} is enough
// and avoids growing a bucket per account that never runs a serving endpoint.
const KVBucketEndpoints = "bedrock-endpoints"

// KVBucketEndpointsHistory pins one revision per key: a superseded state is
// not meaningful to retain, only the current one CAS-guards against races.
const KVBucketEndpointsHistory = 1

// KVBucketLeader holds the reaper's leader lease. Its TTL is what makes the
// lease self-healing: a leader that dies without releasing it is replaced
// within one TTL rather than never.
const (
	KVBucketLeader    = "bedrock-leader"
	KVBucketLeaderTTL = 60 * time.Second
	leaderKey         = "reaper"
)

// missingJetStream is the error every accessor reports when the Service was
// built without a NATS connection to derive a JetStream client from.
const missingJetStream = "bedrock service: nil nats connection"

// newLeaderBucket returns the reaper's TTL-backed leader bucket. The lease
// carries no value of its own, so it is a Bucket rather than a Store[T].
func newLeaderBucket(js jetstream.JetStream) *kvstore.Bucket {
	return kvstore.NewBucket(js, kvstore.Config{
		Name:    KVBucketLeader,
		History: 1,
		TTL:     KVBucketLeaderTTL,
		Missing: missingJetStream,
	})
}

// EndpointKey returns the KV key for accountID's modelID endpoint record.
// Model IDs contain ':' (e.g. "meta.llama3-2-1b-instruct-v1:0"), which NATS
// rejects in a KV key, so the segment is base64url-encoded, mirroring
// gateway_bedrock's weightsKey.
func EndpointKey(accountID, modelID string) string {
	return accountID + "/" + base64.RawURLEncoding.EncodeToString([]byte(modelID))
}

// endpointsPrefix returns the KV key prefix under which accountID's endpoint
// records live, for list to enumerate.
func endpointsPrefix(accountID string) string {
	return accountID + "/"
}

// endpointStore is the typed accessor for the shared bedrock-endpoints bucket.
// One per Service, so the get-or-create round trip is paid once rather than on
// every read and write.
type endpointStore struct {
	*kvstore.Store[EndpointRecord]
}

// newEndpointStore returns the endpoints store over js, replicated across
// replicas nodes. A nil js is permitted: every accessor then reports
// missingJetStream rather than panicking.
func newEndpointStore(js jetstream.JetStream, replicas int) *endpointStore {
	return &endpointStore{Store: kvstore.New[EndpointRecord](js, kvstore.Config{
		Name:     KVBucketEndpoints,
		History:  KVBucketEndpointsHistory,
		Replicas: replicas,
		Missing:  missingJetStream,
	})}
}

// get reads key's record, reporting (zero, false, nil) when it is absent.
func (s *endpointStore) get(ctx context.Context, key string) (EndpointRecord, bool, error) {
	rec, _, found, err := s.getRevision(ctx, key)
	return rec, found, err
}

// getRevision is get with the entry revision surfaced too, for the callers
// that follow with a CAS update.
func (s *endpointStore) getRevision(ctx context.Context, key string) (EndpointRecord, uint64, bool, error) {
	rec, rev, err := s.Get(ctx, key)
	if errors.Is(err, kvstore.ErrNotFound) {
		return EndpointRecord{}, 0, false, nil
	}
	if err != nil {
		return EndpointRecord{}, 0, false, err
	}
	return *rec, rev, true, nil
}

// list returns every endpoint record under accountID's key prefix. Used by the
// reaper and eviction candidate search, both of which only ever act on the
// shared platform account's endpoints; listAll is the cross-account view an
// operator listing needs instead.
func (s *endpointStore) list(ctx context.Context, accountID string) ([]EndpointRecord, error) {
	return s.listPrefix(ctx, endpointsPrefix(accountID))
}

// listAll returns every endpoint record in the bucket regardless of which
// account it is keyed under, so an operator listing sees a pinned,
// account-scoped endpoint alongside the shared platform ones.
func (s *endpointStore) listAll(ctx context.Context) ([]EndpointRecord, error) {
	return s.listPrefix(ctx, "")
}

// listPrefix normalises Store.List's nil to an empty slice, so an empty bucket
// — the normal state before any endpoint exists — reads the same as a filtered
// listing that matched nothing.
func (s *endpointStore) listPrefix(ctx context.Context, prefix string) ([]EndpointRecord, error) {
	recs, err := s.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	if recs == nil {
		return []EndpointRecord{}, nil
	}
	return recs, nil
}
