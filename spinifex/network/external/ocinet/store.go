package ocinet

import (
	"context"
	"fmt"
	"maps"
	"sync"

	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// KVBucketOCIBindings holds the public-address → OCI object-pair mapping.
	// A bucket of its own rather than a corner of the static-pool bucket:
	// nothing about it is range math, its records are keyed differently, and
	// mixing the two would make the static allocator's migrations answerable
	// for a schema it knows nothing about.
	KVBucketOCIBindings = "spinifex-oci-bindings"
	// KVBucketOCIBindingsHistory matches the static pool bucket.
	KVBucketOCIBindingsHistory = 5
	// kvCASRetries bounds contention on one pool key.
	kvCASRetries = 10
)

func bucketConfig() kvstore.Config {
	return kvstore.Config{
		Name:     KVBucketOCIBindings,
		History:  KVBucketOCIBindingsHistory,
		Attempts: kvCASRetries,
		Missing:  "ocinet: no JetStream client configured",
		Exhausted: func(key string, attempts int) error {
			return fmt.Errorf("ocinet: pool %s contended after %d attempts", key, attempts)
		},
	}
}

// KVStore is the JetStream-backed Store.
type KVStore struct {
	store *kvstore.Store[Record]
}

var _ Store = (*KVStore)(nil)

// NewKVStore creates the bindings bucket if it is missing.
func NewKVStore(js jetstream.JetStream) *KVStore {
	return &KVStore{store: kvstore.New[Record](js, bucketConfig())}
}

// NewKVStoreOver builds a store over an already-open bucket.
func NewKVStoreOver(kv jetstream.KeyValue) *KVStore {
	return &KVStore{store: kvstore.Over[Record](nil, kv, bucketConfig())}
}

// Mutate implements Store. Upsert rather than Mutate: the first allocation on
// a pool is what creates its record, and Mutate reports ErrNotFound for an
// absent key — so every pool would fail its own first allocation, after OCI
// had already created and billed the pair.
func (s *KVStore) Mutate(ctx context.Context, poolName string, fn func(*Record) (bool, error)) error {
	return s.store.Upsert(ctx, poolName, func(rec *Record) (bool, error) {
		// A record written before the field existed, or a freshly created one,
		// decodes to a nil map that the callers below would panic assigning to.
		if rec.Bindings == nil {
			rec.Bindings = map[string]Binding{}
		}
		return fn(rec)
	})
}

// Get implements Store. A pool with no record yet reports kvstore.ErrNotFound,
// which callers read as an empty binding set — surfacing it rather than hiding
// it keeps a bucket that has gone missing distinguishable from a pool that has
// simply never allocated.
func (s *KVStore) Get(ctx context.Context, poolName string) (Record, error) {
	rec, _, err := s.store.Get(ctx, poolName)
	if err != nil {
		return Record{}, err
	}
	if rec == nil {
		return Record{Bindings: map[string]Binding{}}, nil
	}
	if rec.Bindings == nil {
		rec.Bindings = map[string]Binding{}
	}
	return *rec, nil
}

// MemStore is an in-memory Store for tests. Concurrency-safe so a test can
// exercise the CAS-retry paths without NATS.
type MemStore struct {
	mu      sync.Mutex
	records map[string]Record
	// mutateErr, when set, fails every Mutate. Lets a test drive the paths
	// where OCI has already been changed and only the record write fails.
	mutateErr error
}

var _ Store = (*MemStore)(nil)

// NewMemStore returns an empty in-memory store.
func NewMemStore() *MemStore { return &MemStore{records: map[string]Record{}} }

// FailMutate makes every subsequent Mutate return err.
func (m *MemStore) FailMutate(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mutateErr = err
}

// Mutate implements Store.
func (m *MemStore) Mutate(_ context.Context, poolName string, fn func(*Record) (bool, error)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mutateErr != nil {
		return m.mutateErr
	}
	rec := m.copyLocked(poolName)
	changed, err := fn(&rec)
	if err != nil {
		return err
	}
	if changed {
		m.records[poolName] = rec
	}
	return nil
}

// Get implements Store, including the not-found error the KV store returns for
// a pool that has never allocated. Returning an empty record instead made the
// fake disagree with the real store on the one case every pool starts in.
func (m *MemStore) Get(_ context.Context, poolName string) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.records[poolName]; !ok {
		return Record{}, fmt.Errorf("%w: %s", kvstore.ErrNotFound, poolName)
	}
	return m.copyLocked(poolName), nil
}

// copyLocked returns a deep-enough copy that a caller mutating the returned
// record cannot reach into the stored one — the failure a map would otherwise
// hand out by reference, making a rolled-back Mutate look like it committed.
func (m *MemStore) copyLocked(poolName string) Record {
	rec := Record{Bindings: map[string]Binding{}}
	maps.Copy(rec.Bindings, m.records[poolName].Bindings)
	return rec
}
