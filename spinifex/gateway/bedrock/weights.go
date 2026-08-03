package gateway_bedrock

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/nats-io/nats.go/jetstream"
)

// bedrockWeightsBucket is the cluster-replicated KV bucket holding, per
// model, this deployment's staged weights artifact. Two deployments of the
// same catalog can stage different snapshots, or none.
const bedrockWeightsBucket = "bedrock-weights"

// bedrockWeightsHistory keeps one revision; a re-stage overwrites in place.
const bedrockWeightsHistory = 1

// weightsKey returns the KV key for modelID's staged-weights record.
// Model IDs contain ':' (e.g. "meta.llama3-2-1b-instruct-v1:0"), which NATS
// rejects in a KV key, so the segment is base64url-encoded.
func weightsKey(modelID string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(modelID))
}

// weightsRecord is the JSON value stored under weightsKey. SourceURI keeps
// the record self-describing -- 'ochre weights stage' uses it to detect a
// no-op re-stage, and 'ochre weights list' surfaces it so an operator can
// see where a snapshot came from without a side channel.
type weightsRecord struct {
	SnapshotID string `json:"snapshot_id"`
	SourceURI  string `json:"source_uri"`
}

// WeightsResolver resolves a self-hosted model's deployment-specific weights
// snapshot ID. A model with no resolvable snapshot has nothing to serve it
// with and must not be advertised — see tieredCatalog and GetFoundationModel.
type WeightsResolver interface {
	Resolve(ctx context.Context, modelID string) (snapshotID string, ok bool, err error)
}

// WeightsStore resolves per-model weights records from the bedrock-weights
// JetStream KV bucket.
type WeightsStore struct {
	js       jetstream.JetStream
	replicas int

	mu sync.Mutex
	kv jetstream.KeyValue
}

var _ WeightsResolver = (*WeightsStore)(nil)

// NewWeightsStore constructs a WeightsStore over the cluster's JetStream
// client, replicated across replicas nodes.
func NewWeightsStore(js jetstream.JetStream, replicas int) *WeightsStore {
	return &WeightsStore{js: js, replicas: replicas}
}

// bucket lazily opens (or creates) the cluster-replicated bedrock-weights KV
// bucket, caching the handle for subsequent calls, mirroring
// CredentialStore.bucket.
func (s *WeightsStore) bucket(ctx context.Context) (jetstream.KeyValue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kv != nil {
		return s.kv, nil
	}
	kv, err := kvutil.GetOrCreateBucketWithReplicas(ctx, s.js, bedrockWeightsBucket, bedrockWeightsHistory, s.replicas)
	if err != nil {
		return nil, err
	}
	s.kv = kv
	return kv, nil
}

// getRecord reads and decodes modelID's weightsRecord. ok is false on a KV
// miss (not staged); a malformed record is reported as an error rather than
// silently treated as unstaged, since that would mask real corruption.
func (s *WeightsStore) getRecord(ctx context.Context, modelID string) (weightsRecord, bool, error) {
	kv, err := s.bucket(ctx)
	if err != nil {
		return weightsRecord{}, false, err
	}
	entry, err := kv.Get(ctx, weightsKey(modelID))
	switch {
	case err == nil:
		var rec weightsRecord
		if jsonErr := json.Unmarshal(entry.Value(), &rec); jsonErr != nil {
			return weightsRecord{}, false, fmt.Errorf("decode weights record for %s: %w", modelID, jsonErr)
		}
		return rec, true, nil
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return weightsRecord{}, false, nil
	default:
		return weightsRecord{}, false, fmt.Errorf("kv get weights record for %s: %w", modelID, err)
	}
}

// Resolve returns modelID's staged weights snapshot ID, if one has been set.
func (s *WeightsStore) Resolve(ctx context.Context, modelID string) (string, bool, error) {
	rec, ok, err := s.getRecord(ctx, modelID)
	if err != nil || !ok {
		return "", ok, err
	}
	return rec.SnapshotID, true, nil
}

// GetWeights returns modelID's full staged-weights record (source URI and
// snapshot ID), if one has been set. 'ochre weights stage' uses this to
// decide idempotency and to report the snapshot a re-stage is replacing.
func (s *WeightsStore) GetWeights(ctx context.Context, modelID string) (WeightsEntry, bool, error) {
	rec, ok, err := s.getRecord(ctx, modelID)
	if err != nil || !ok {
		return WeightsEntry{}, ok, err
	}
	return WeightsEntry{ModelID: modelID, SourceURI: rec.SourceURI, SnapshotID: rec.SnapshotID}, true, nil
}

// PutWeights records modelID's staged weights artifact: the snapshot ID
// endpoints COW-clone from, and the source S3 URI it was materialised from.
func (s *WeightsStore) PutWeights(ctx context.Context, modelID, sourceURI, snapshotID string) error {
	kv, err := s.bucket(ctx)
	if err != nil {
		return err
	}
	value, err := json.Marshal(weightsRecord{SnapshotID: snapshotID, SourceURI: sourceURI})
	if err != nil {
		return fmt.Errorf("encode weights record for %s: %w", modelID, err)
	}
	if _, err := kv.Put(ctx, weightsKey(modelID), value); err != nil {
		return fmt.Errorf("kv put weights record for %s: %w", modelID, err)
	}
	return nil
}

// WeightsEntry is one staged model's KV record, decoded back from
// weightsKey's base64url-encoded modelID for operator-facing listing.
type WeightsEntry struct {
	ModelID    string
	SourceURI  string
	SnapshotID string
}

// ListWeights returns every staged model and its record, sorted by model
// ID, so 'ochre weights list' can show an operator what's staged, where it
// came from, and why a model is (or isn't) advertised via
// ListFoundationModels.
func (s *WeightsStore) ListWeights(ctx context.Context) ([]WeightsEntry, error) {
	kv, err := s.bucket(ctx)
	if err != nil {
		return nil, err
	}
	keys, err := kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("kv list weights keys: %w", err)
	}

	entries := make([]WeightsEntry, 0, len(keys))
	for _, key := range keys {
		modelID, err := base64.RawURLEncoding.DecodeString(key)
		if err != nil {
			// Not a key weightsKey wrote; skip rather than fail the whole list.
			continue
		}
		entry, err := kv.Get(ctx, key)
		if err != nil {
			continue
		}
		var rec weightsRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			continue
		}
		entries = append(entries, WeightsEntry{ModelID: string(modelID), SourceURI: rec.SourceURI, SnapshotID: rec.SnapshotID})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ModelID < entries[j].ModelID })
	return entries, nil
}

// DeleteWeights drops modelID's staged-weights KV entry only. The backing
// volume, snapshot and source S3 objects are left intact — reclaiming that
// storage is a separate, explicit act.
func (s *WeightsStore) DeleteWeights(ctx context.Context, modelID string) error {
	kv, err := s.bucket(ctx)
	if err != nil {
		return err
	}
	if err := kv.Delete(ctx, weightsKey(modelID)); err != nil {
		return fmt.Errorf("kv delete weights record for %s: %w", modelID, err)
	}
	return nil
}

// noopWeightsResolver resolves no snapshot for any model.
type noopWeightsResolver struct{}

var _ WeightsResolver = (*noopWeightsResolver)(nil)

func (noopWeightsResolver) Resolve(_ context.Context, _ string) (string, bool, error) {
	return "", false, nil
}

// NoopWeightsResolver resolves no weights for any model: the unconfigured
// direction is "no self-host model is servable", matching how
// NoopCredentialResolver resolves no provider credentials.
var NoopWeightsResolver WeightsResolver = noopWeightsResolver{}

// weightsResolverMu guards weightsResolver, process-wide state set once at
// service start via SetWeightsResolver.
var (
	weightsResolverMu sync.RWMutex
	weightsResolver   WeightsResolver = NoopWeightsResolver
)

// SetWeightsResolver installs the resolver tieredCatalog and
// GetFoundationModel gate self-host entries on. A nil resolver restores the
// no-op default.
func SetWeightsResolver(r WeightsResolver) {
	weightsResolverMu.Lock()
	defer weightsResolverMu.Unlock()
	if r == nil {
		r = NoopWeightsResolver
	}
	weightsResolver = r
}

// currentWeightsResolver returns the resolver installed by SetWeightsResolver.
func currentWeightsResolver() WeightsResolver {
	weightsResolverMu.RLock()
	defer weightsResolverMu.RUnlock()
	return weightsResolver
}
