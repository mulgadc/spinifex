package gateway_bedrock

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"

	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/nats-io/nats.go/jetstream"
)

// bedrockWeightsBucket is the cluster-replicated KV bucket holding, per
// model, the snapshot ID of this deployment's staged weights volume. The
// tree defines what a self-host model needs (catalogEntry.MinVRAMMiB,
// InstanceType, VLLMArgs); which artifact actually serves it is deployment
// state, since two deployments of the same catalog can stage different
// snapshots (or none at all).
const bedrockWeightsBucket = "bedrock-weights"

// bedrockWeightsHistory keeps one revision; a re-stage overwrites in place.
const bedrockWeightsHistory = 1

// weightsKey returns the KV key for modelID's staged-weights snapshot.
// Model IDs contain ':' (e.g. "meta.llama3-2-1b-instruct-v1:0"), which NATS
// rejects in a KV key, so the segment is base64url-encoded.
func weightsKey(modelID string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(modelID))
}

// WeightsResolver resolves a self-hosted model's deployment-specific weights
// snapshot ID. A model with no resolvable snapshot has nothing to serve it
// with and must not be advertised — see tieredCatalog and GetFoundationModel.
type WeightsResolver interface {
	Resolve(ctx context.Context, modelID string) (snapshotID string, ok bool, err error)
}

// WeightsStore resolves per-model weights snapshot IDs from the
// bedrock-weights JetStream KV bucket.
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

// Resolve returns modelID's staged weights snapshot ID, if one has been set.
func (s *WeightsStore) Resolve(ctx context.Context, modelID string) (string, bool, error) {
	kv, err := s.bucket(ctx)
	if err != nil {
		return "", false, err
	}
	entry, err := kv.Get(ctx, weightsKey(modelID))
	switch {
	case err == nil:
		return string(entry.Value()), true, nil
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return "", false, nil
	default:
		return "", false, fmt.Errorf("kv get weights snapshot for %s: %w", modelID, err)
	}
}

// PutWeights records snapshotID as modelID's staged weights artifact.
func (s *WeightsStore) PutWeights(ctx context.Context, modelID, snapshotID string) error {
	kv, err := s.bucket(ctx)
	if err != nil {
		return err
	}
	if _, err := kv.Put(ctx, weightsKey(modelID), []byte(snapshotID)); err != nil {
		return fmt.Errorf("kv put weights snapshot for %s: %w", modelID, err)
	}
	return nil
}

// noopWeightsResolver resolves no snapshot for any model.
type noopWeightsResolver struct{}

var _ WeightsResolver = (*noopWeightsResolver)(nil)

func (noopWeightsResolver) Resolve(_ context.Context, _ string) (string, bool, error) {
	return "", false, nil
}

// NoopWeightsResolver resolves no weights for any model. It is the fallback
// wherever no WeightsStore is configured: the unconfigured direction is "no
// self-host model is servable", not "every self-host model is", matching how
// NoopCredentialResolver resolves no provider credentials.
var NoopWeightsResolver WeightsResolver = noopWeightsResolver{}

// weightsResolverMu guards weightsResolver, process-wide runtime
// configuration set once at service start (SetWeightsResolver) rather than
// threaded as a parameter, since ListFoundationModels and GetFoundationModel
// are called through a fixed-arity route table.
var (
	weightsResolverMu sync.RWMutex
	weightsResolver   WeightsResolver = NoopWeightsResolver
)

// SetWeightsResolver installs the WeightsResolver that tieredCatalog and
// GetFoundationModel use to gate self-host entries on a resolvable weights
// snapshot. Call once during service construction; a nil resolver restores
// the no-op default.
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
