package floating

import (
	"context"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
)

const (
	// KVBucketFloatingBindings holds the address → provider address mapping for
	// every floating pool, keyed by pool name.
	KVBucketFloatingBindings        = "spinifex-floating-bindings"
	KVBucketFloatingBindingsHistory = 5
	kvCASRetries                    = 10
)

func bucketConfig() kvstore.Config {
	return kvstore.Config{
		Name:     KVBucketFloatingBindings,
		History:  KVBucketFloatingBindingsHistory,
		Attempts: kvCASRetries,
		Missing:  "floating: no JetStream client configured",
		Exhausted: func(key string, attempts int) error {
			return fmt.Errorf("floating: pool %s contended after %d attempts", key, attempts)
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

// Mutate implements Store. Upsert, because a pool's first allocation is what
// creates its record, and that allocation has already made a billable address.
func (s *KVStore) Mutate(ctx context.Context, poolName string, fn func(*Record) (bool, error)) error {
	return s.store.Upsert(ctx, poolName, func(rec *Record) (bool, error) {
		if rec.Bindings == nil {
			rec.Bindings = map[string]Binding{}
		}
		return fn(rec)
	})
}

// Get implements Store. A pool with no record reports kvstore.ErrNotFound,
// which callers read as an empty binding set.
func (s *KVStore) Get(ctx context.Context, poolName string) (Record, error) {
	rec, _, err := s.store.Get(ctx, poolName)
	if err != nil {
		return Record{}, err
	}
	if rec == nil || rec.Bindings == nil {
		return Record{Bindings: map[string]Binding{}}, nil
	}
	return *rec, nil
}
