package testutil

import (
	"context"
	"sync"

	"github.com/nats-io/nats.go/jetstream"
)

// StaleReadKV wraps a bucket so that the first Get of a key after a write to it
// returns the entry as it stood before that write, as a lagging replica does
// when it answers a direct get. Later Gets see the current entry.
type StaleReadKV struct {
	jetstream.KeyValue

	mu    sync.Mutex
	stale map[string]jetstream.KeyValueEntry
}

var _ jetstream.KeyValue = (*StaleReadKV)(nil)

// NewStaleReadKV wraps kv.
func NewStaleReadKV(kv jetstream.KeyValue) *StaleReadKV {
	return &StaleReadKV{KeyValue: kv, stale: map[string]jetstream.KeyValueEntry{}}
}

// remember captures the entry about to be overwritten.
func (k *StaleReadKV) remember(ctx context.Context, key string) jetstream.KeyValueEntry {
	prev, err := k.KeyValue.Get(ctx, key)
	if err != nil {
		return nil
	}
	return prev
}

func (k *StaleReadKV) arm(key string, prev jetstream.KeyValueEntry) {
	if prev == nil {
		return
	}
	k.mu.Lock()
	k.stale[key] = prev
	k.mu.Unlock()
}

func (k *StaleReadKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	k.mu.Lock()
	prev, ok := k.stale[key]
	delete(k.stale, key)
	k.mu.Unlock()
	if ok {
		return prev, nil
	}
	return k.KeyValue.Get(ctx, key)
}

func (k *StaleReadKV) Update(ctx context.Context, key string, value []byte, last uint64) (uint64, error) {
	prev := k.remember(ctx, key)
	rev, err := k.KeyValue.Update(ctx, key, value, last)
	if err == nil {
		k.arm(key, prev)
	}
	return rev, err
}

func (k *StaleReadKV) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	prev := k.remember(ctx, key)
	rev, err := k.KeyValue.Put(ctx, key, value)
	if err == nil {
		k.arm(key, prev)
	}
	return rev, err
}

// RejectingKV wraps a bucket so that writes for which reject returns an error
// fail with it, and every other operation passes through.
type RejectingKV struct {
	jetstream.KeyValue

	reject func(key string, value []byte) error
}

var _ jetstream.KeyValue = (*RejectingKV)(nil)

// NewRejectingKV wraps kv. reject runs before each Put, Create and Update.
func NewRejectingKV(kv jetstream.KeyValue, reject func(key string, value []byte) error) *RejectingKV {
	return &RejectingKV{KeyValue: kv, reject: reject}
}

func (k *RejectingKV) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	if err := k.reject(key, value); err != nil {
		return 0, err
	}
	return k.KeyValue.Put(ctx, key, value)
}

func (k *RejectingKV) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	if err := k.reject(key, value); err != nil {
		return 0, err
	}
	return k.KeyValue.Create(ctx, key, value, opts...)
}

func (k *RejectingKV) Update(ctx context.Context, key string, value []byte, last uint64) (uint64, error) {
	if err := k.reject(key, value); err != nil {
		return 0, err
	}
	return k.KeyValue.Update(ctx, key, value, last)
}
