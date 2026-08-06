package handlers_bedrock

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/kvutil"
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

// GetOrCreateLeaderBucket returns the reaper's TTL-backed leader bucket.
func GetOrCreateLeaderBucket(ctx context.Context, js jetstream.JetStream) (jetstream.KeyValue, error) {
	kv, err := kvutil.GetOrCreateBucketWithTTL(ctx, js, KVBucketLeader, 1, KVBucketLeaderTTL)
	if err != nil {
		return nil, fmt.Errorf("bedrock: create leader KV bucket: %w", err)
	}
	return kv, nil
}

// EndpointKey returns the KV key for accountID's modelID endpoint record.
// Model IDs contain ':' (e.g. "meta.llama3-2-1b-instruct-v1:0"), which NATS
// rejects in a KV key, so the segment is base64url-encoded, mirroring
// gateway_bedrock's weightsKey.
func EndpointKey(accountID, modelID string) string {
	return accountID + "/" + base64.RawURLEncoding.EncodeToString([]byte(modelID))
}

// endpointsPrefix returns the KV key prefix under which accountID's endpoint
// records live, for ListEndpoints to enumerate.
func endpointsPrefix(accountID string) string {
	return accountID + "/"
}

// GetOrCreateEndpointsBucket returns the shared bedrock-endpoints KV bucket,
// creating it on first use at the given replica count (clamped to a minimum
// of 1).
func GetOrCreateEndpointsBucket(ctx context.Context, js jetstream.JetStream, replicas int) (jetstream.KeyValue, error) {
	kv, err := kvutil.GetOrCreateBucketWithReplicas(ctx, js, KVBucketEndpoints, KVBucketEndpointsHistory, replicas)
	if err != nil {
		return nil, fmt.Errorf("bedrock: create endpoints KV bucket: %w", err)
	}
	return kv, nil
}

// Returns (false, nil) when the key is absent.
func getJSON(ctx context.Context, kv jetstream.KeyValue, key string, out any) (bool, error) {
	entry, err := kv.Get(ctx, key)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(entry.Value(), out); err != nil {
		return false, fmt.Errorf("unmarshal %s: %w", key, err)
	}
	return true, nil
}

// getJSON plus the entry revision, for callers that follow with a CAS update.
func getJSONRevision(ctx context.Context, kv jetstream.KeyValue, key string, out any) (uint64, bool, error) {
	entry, err := kv.Get(ctx, key)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if err := json.Unmarshal(entry.Value(), out); err != nil {
		return 0, false, fmt.Errorf("unmarshal %s: %w", key, err)
	}
	return entry.Revision(), true, nil
}

// createJSONRevision writes v at key only if nothing is stored there,
// returning the created entry's revision. Returns jetstream.ErrKeyExists when
// the key is already taken — the caller's cue to fall back to the read+CAS path.
func createJSONRevision(ctx context.Context, kv jetstream.KeyValue, key string, v any) (uint64, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return 0, err
	}
	rev, err := kv.Create(ctx, key, data)
	if err != nil {
		return 0, err
	}
	return rev, nil
}

// updateJSON writes v at key only if the stored entry is still at rev — the
// CAS primitive single-flight and state transitions build on.
func updateJSON(ctx context.Context, kv jetstream.KeyValue, key string, rev uint64, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := kv.Update(ctx, key, data, rev); err != nil {
		return fmt.Errorf("update %s: %w", key, err)
	}
	return nil
}

// deleteJSON removes key unconditionally. Purge (not just Delete) so no
// stale revision survives to confuse a subsequent createJSONRevision's
// "does the key exist" check under History=1.
func deleteJSON(ctx context.Context, kv jetstream.KeyValue, key string) error {
	if err := kv.Purge(ctx, key); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return fmt.Errorf("purge %s: %w", key, err)
	}
	return nil
}

// ListEndpoints returns every endpoint record under accountID's key prefix.
func ListEndpoints(ctx context.Context, kv jetstream.KeyValue, accountID string) ([]EndpointRecord, error) {
	keys, err := kvutil.Keys(ctx, kv)
	if err != nil {
		// An empty bucket is the normal state before any endpoint has ever
		// been created, not a failure, matching every other kvutil.Keys caller.
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return []EndpointRecord{}, nil
		}
		return nil, fmt.Errorf("bedrock: list endpoint keys: %w", err)
	}
	prefix := endpointsPrefix(accountID)
	out := make([]EndpointRecord, 0, len(keys))
	for _, key := range keys {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		var rec EndpointRecord
		ok, getErr := getJSON(ctx, kv, key, &rec)
		if getErr != nil {
			return nil, fmt.Errorf("bedrock: read endpoint %s: %w", key, getErr)
		}
		if !ok {
			// Deleted between Keys() and Get() — not an error, just gone.
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}
