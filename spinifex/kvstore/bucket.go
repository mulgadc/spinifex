// Package kvstore provides a typed store over a single JetStream KV bucket:
// one memoised get-or-create accessor and a JSON codec for the record type,
// replacing the accessor each caller used to hand-roll.
package kvstore

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/nats-io/nats.go/jetstream"
)

// Config describes the bucket that a Store or Bucket sits over.
type Config struct {
	Name      string
	History   int
	Replicas  int
	TTL       time.Duration
	Missing   string
	Attempts  int
	Exhausted func(key string, attempts int) error
}

// Bucket memoises a lazily created KV bucket. Construct it with NewBucket.
type Bucket struct {
	js  jetstream.JetStream
	cfg Config

	mu sync.Mutex
	kv jetstream.KeyValue
}

// NewBucket returns a Bucket over js. A nil js is permitted: every call to KV then
// fails with cfg.Missing rather than panicking.
func NewBucket(js jetstream.JetStream, cfg Config) *Bucket {
	return &Bucket{js: js, cfg: cfg}
}

// NewOpenBucket returns a Bucket over an already-open handle, for callers that
// resolve their bucket at construction so a bad connection fails at startup
// rather than on first use.
func NewOpenBucket(kv jetstream.KeyValue, cfg Config) *Bucket {
	return &Bucket{kv: kv, cfg: cfg}
}

// Configured reports whether the bucket has anything to open against, for the
// callers whose absent-KV path is a legitimate fallback rather than an error.
func (b *Bucket) Configured() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.js != nil || b.kv != nil
}

// KV opens the bucket on first use and returns the cached handle thereafter.
func (b *Bucket) KV(ctx context.Context) (jetstream.KeyValue, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.kv != nil {
		return b.kv, nil
	}
	if b.js == nil {
		if b.cfg.Missing == "" {
			return nil, errors.New("kvstore: no JetStream client configured")
		}
		return nil, errors.New(b.cfg.Missing)
	}
	kv, err := b.open(ctx)
	if err != nil {
		return nil, err
	}
	b.kv = kv
	return kv, nil
}

// open picks the kvutil helper matching the configured TTL and replica count.
func (b *Bucket) open(ctx context.Context) (jetstream.KeyValue, error) {
	switch {
	case b.cfg.TTL > 0:
		return kvutil.GetOrCreateBucketWithTTL(ctx, b.js, b.cfg.Name, b.cfg.History, b.cfg.TTL)
	case b.cfg.Replicas > 0:
		return kvutil.GetOrCreateBucketWithReplicas(ctx, b.js, b.cfg.Name, b.cfg.History, b.cfg.Replicas)
	default:
		return kvutil.GetOrCreateBucket(ctx, b.js, b.cfg.Name, b.cfg.History)
	}
}
