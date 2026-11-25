package consolebranding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
)

// Bucket and Key address the one branding record. History is 1: a save
// replaces the record and no earlier version is worth keeping.
const (
	Bucket = "spinifex-console-branding"
	Key    = "config"
)

// ErrStaleRevision reports a write whose revision is not the stored one.
var ErrStaleRevision = errors.New("branding record has changed since it was read")

// OpenBucket opens, or creates, the branding bucket.
func OpenBucket(ctx context.Context, js jetstream.KeyValueManager) (jetstream.KeyValue, error) {
	return kvstore.GetOrCreateBucket(ctx, js, Bucket, 1)
}

// KV is the part of a jetstream.KeyValue the store uses.
type KV interface {
	Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error)
	Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error)
	Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error)
}

var _ KV = (jetstream.KeyValue)(nil)

// Store reads and writes the branding record.
type Store struct {
	KV KV
}

// Get returns the stored record and its revision, or nil and 0 when none is set.
func (s Store) Get(ctx context.Context) (*Record, uint64, error) {
	entry, err := s.KV.Get(ctx, Key)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("read branding record: %w", err)
	}
	var rec Record
	if err := json.Unmarshal(entry.Value(), &rec); err != nil {
		return nil, 0, fmt.Errorf("decode branding record: %w", err)
	}
	return &rec, entry.Revision(), nil
}

// Put writes rec if the stored revision is still revision, where 0 means no
// record exists. It returns the new revision, or ErrStaleRevision.
func (s Store) Put(ctx context.Context, rec Record, revision uint64) (uint64, error) {
	data, err := json.Marshal(rec)
	if err != nil {
		return 0, fmt.Errorf("encode branding record: %w", err)
	}
	var next uint64
	if revision == 0 {
		next, err = s.KV.Create(ctx, Key, data)
	} else {
		next, err = s.KV.Update(ctx, Key, data, revision)
	}
	if kvstore.IsRevisionConflict(err) {
		return 0, ErrStaleRevision
	}
	if err != nil {
		return 0, fmt.Errorf("write branding record: %w", err)
	}
	return next, nil
}

// Cache is the gateway's copy of the record. Readers never wait on NATS: a
// failed refresh keeps the last record, or none if none has loaded.
type Cache struct {
	mu       sync.RWMutex
	rec      *Record
	revision uint64
}

// Snapshot returns the cached record and its revision.
func (c *Cache) Snapshot() (*Record, uint64) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.rec, c.revision
}

// Set stores a record read at revision. An older revision than the cached one
// is ignored, so a slow refresh cannot undo a newer local write.
func (c *Cache) Set(rec *Record, revision uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if revision != 0 && revision < c.revision {
		return
	}
	c.rec, c.revision = rec, revision
}

// Refresh reads the record once into the cache.
func (c *Cache) Refresh(ctx context.Context, s Store) error {
	rec, rev, err := s.Get(ctx)
	if err != nil {
		return err
	}
	c.Set(rec, rev)
	return nil
}

// Run refreshes the cache every interval until ctx ends. Each read is bounded
// so a stalled NATS connection cannot stop later refreshes.
func (c *Cache) Run(ctx context.Context, s Store, interval time.Duration) {
	refresh := func() {
		rctx, cancel := context.WithTimeout(ctx, interval)
		defer cancel()
		if err := c.Refresh(rctx, s); err != nil && ctx.Err() == nil {
			slog.Warn("Console branding: refresh failed; keeping the last record", "err", err)
		}
	}
	refresh()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			refresh()
		}
	}
}
