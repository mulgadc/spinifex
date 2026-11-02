package kvstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/nats-io/nats.go/jetstream"
)

// Sentinels for the two conditions every caller distinguishes. Callers map
// these onto their own package errors.
var (
	ErrNotFound = errors.New("kvstore: key not found")
	ErrExists   = errors.New("kvstore: key already exists")
	ErrConflict = errors.New("kvstore: revision conflict")
)

// Store is a Bucket plus a JSON codec for T.
type Store[T any] struct {
	*Bucket
}

// New returns a Store over js for records of type T.
func New[T any](js jetstream.JetStream, cfg Config) *Store[T] {
	return &Store[T]{Bucket: NewBucket(js, cfg)}
}

// Get reads and decodes one record, returning ErrNotFound when the key is
// absent. The revision is returned for callers doing their own CAS.
func (s *Store[T]) Get(ctx context.Context, key string) (*T, uint64, error) {
	var (
		v   *T
		rev uint64
	)
	err := s.withKV(ctx, func(kv jetstream.KeyValue) error {
		var err error
		v, rev, err = s.get(ctx, kv, key)
		return err
	})
	return v, rev, err
}

// Create writes a record only if the key is absent, returning ErrExists when
// it is not. The create-only write is the single-writer claim, and the
// returned revision is the CAS precondition for the claim's own first update.
func (s *Store[T]) Create(ctx context.Context, key string, v *T) (uint64, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return 0, fmt.Errorf("kvstore: encode %s: %w", key, err)
	}
	// Wrapped inside the closure, so a bucket that could not be opened at all
	// reports the configured reason rather than a create failure on top of it.
	var rev uint64
	err = s.withKV(ctx, func(kv jetstream.KeyValue) error {
		var createErr error
		if rev, createErr = kv.Create(ctx, key, data); createErr != nil {
			return fmt.Errorf("kvstore: create %s: %w", key, createErr)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return 0, fmt.Errorf("%w: %s", ErrExists, key)
		}
		return 0, err
	}
	return rev, nil
}

// Mutate applies fn under a revision-guarded retry loop. fn reports whether it
// changed anything; false commits no write. An absent key is ErrNotFound.
func (s *Store[T]) Mutate(ctx context.Context, key string, fn func(*T) (bool, error)) error {
	// Safe to re-run: Update re-reads the record inside its own CAS loop, so
	// the second attempt starts from whatever the reopened bucket holds.
	return s.withKV(ctx, func(kv jetstream.KeyValue) error {
		_, err := kvutil.Update(ctx, kv, key, kvutil.CASConfig{
			Attempts:  s.cfg.Attempts,
			NotFound:  fmt.Errorf("%w: %s", ErrNotFound, key),
			Exhausted: s.cfg.Exhausted,
		}, fn)
		return err
	})
}

// Upsert is Mutate for a counter-style record whose first write creates it: an
// absent key starts fn from the zero value rather than reporting ErrNotFound.
func (s *Store[T]) Upsert(ctx context.Context, key string, fn func(*T) (bool, error)) error {
	return s.withKV(ctx, func(kv jetstream.KeyValue) error {
		_, err := kvutil.Update(ctx, kv, key, kvutil.CASConfig{
			Attempts:       s.cfg.Attempts,
			CreateIfAbsent: true,
			Exhausted:      s.cfg.Exhausted,
		}, fn)
		return err
	})
}

// List decodes every record whose key carries the given prefix, in key order.
// An empty prefix matches everything and an empty bucket is not an error. A
// key deleted before the read is absent.
func (s *Store[T]) List(ctx context.Context, prefix string) ([]T, error) {
	var out []T
	// Whole read sits inside the wrapper: a stream lost partway has to restart
	// the read, so out is reset rather than appended to twice.
	err := s.withKV(ctx, func(kv jetstream.KeyValue) error {
		out = nil
		latest, _, err := s.readRange(ctx, kv, ">", false)
		if err != nil {
			return err
		}
		keys := make([]string, 0, len(latest))
		for key, entry := range latest {
			if strings.HasPrefix(key, prefix) && live(entry) {
				keys = append(keys, key)
			}
		}
		slices.Sort(keys)
		for _, key := range keys {
			var v T
			if err := json.Unmarshal(latest[key].Value(), &v); err != nil {
				return fmt.Errorf("kvstore: decode %s: %w", key, err)
			}
			out = append(out, v)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Item is one record from a Snapshot, carrying the revision it was read at so
// the caller can CAS against it.
type Item[T any] struct {
	Key      string
	Value    T
	Revision uint64
}

// Snapshot reads every record matching filter in one ordered pass and reports
// the highest stream sequence it saw. Filter takes NATS subject wildcards.
//
// One consumer answers the whole read, so the keys and the values are
// mutually consistent, and the read ends only once the stream leader has
// nothing in range newer than what was delivered.
//
// A deleted key is not returned, but its revision still counts towards
// highWater. That is deliberate: the mark says how far this reader got
// through the stream, not what survives in it, and a delete is how far a
// reader gets when the newest thing that happened was a delete.
func (s *Store[T]) Snapshot(ctx context.Context, filter string) (items []Item[T], highWater uint64, err error) {
	// Pure read, so it is safe for withKV to run it twice; the results are
	// reset on entry rather than appended to across a retry.
	err = s.withKV(ctx, func(kv jetstream.KeyValue) error {
		items, highWater = nil, 0
		latest, mark, err := s.readRange(ctx, kv, filter, false)
		if err != nil {
			return err
		}
		highWater = mark
		keys := make([]string, 0, len(latest))
		for key, entry := range latest {
			if live(entry) {
				keys = append(keys, key)
			}
		}
		slices.Sort(keys)
		for _, key := range keys {
			entry := latest[key]
			var v T
			if err := json.Unmarshal(entry.Value(), &v); err != nil {
				return fmt.Errorf("kvstore: decode %s: %w", key, err)
			}
			items = append(items, Item[T]{Key: key, Value: v, Revision: entry.Revision()})
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return items, highWater, nil
}

// DeletePrefix removes every key carrying the given prefix. An empty prefix
// purges the bucket. Idempotent, like Delete.
func (s *Store[T]) DeletePrefix(ctx context.Context, prefix string) error {
	// Safe to re-run: a key deleted before the stream was lost is already
	// absent on the retry, which Delete treats as success.
	return s.withKV(ctx, func(kv jetstream.KeyValue) error {
		latest, _, err := s.readRange(ctx, kv, ">", true)
		if err != nil {
			return err
		}
		for key, entry := range latest {
			if !strings.HasPrefix(key, prefix) || !live(entry) {
				continue
			}
			if err := kv.Delete(ctx, key); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
				return fmt.Errorf("kvstore: delete %s: %w", key, err)
			}
		}
		return nil
	})
}

// Over returns a Store for records of type T over an already-open bucket. js is
// what recovery reopens against; pass nil to keep the handle fixed.
func Over[T any](js jetstream.JetStream, kv jetstream.KeyValue, cfg Config) *Store[T] {
	return &Store[T]{Bucket: NewOpenBucket(js, kv, cfg)}
}

// On returns a Store for records of type T over an existing bucket, for a
// bucket holding more than one record type. Every view shares the bucket's
// handle, so they open once between them and one view's recovery repairs all.
func On[T any](b *Bucket) *Store[T] {
	return &Store[T]{Bucket: b}
}

// Claim atomically reads a record and deletes it, so at most one caller across
// the cluster can win it. notFound reports that there was nothing to claim,
// which is the losing racer's answer rather than an error.
func (s *Store[T]) Claim(ctx context.Context, key string) (v *T, notFound bool, err error) {
	// Safe to re-run: the read and the revision-guarded delete both happen
	// inside kvutil.Claim, so a second attempt starts from the reopened bucket.
	err = s.withKV(ctx, func(kv jetstream.KeyValue) error {
		var claimErr error
		v, notFound, claimErr = kvutil.Claim[T](ctx, kv, key, kvutil.CASConfig{
			Attempts:  s.cfg.Attempts,
			Exhausted: s.cfg.Exhausted,
		})
		return claimErr
	})
	if err != nil {
		return nil, false, err
	}
	return v, notFound, nil
}

// Replace writes v over whatever key holds, under the store's CAS loop. Unlike
// Set, which is a plain last-write-wins put, a write that loses a race here is
// retried against the winner's revision rather than landing out of order. The
// record is replaced wholesale, so unlike Mutate the caller needs no read and v
// need not be copyable.
func (s *Store[T]) Replace(ctx context.Context, key string, v *T) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("kvstore: encode %s: %w", key, err)
	}
	// Safe to re-run: kvutil.Put re-reads the revision inside its own loop.
	return s.withKV(ctx, func(kv jetstream.KeyValue) error {
		if err := kvutil.Put(ctx, kv, key, kvutil.CASConfig{
			CreateIfAbsent: true,
			Attempts:       s.cfg.Attempts,
			Exhausted:      s.cfg.Exhausted,
		}, data); err != nil {
			return fmt.Errorf("kvstore: replace %s: %w", key, err)
		}
		return nil
	})
}

// Set writes a record whether or not the key exists, for callers whose write
// is a replacement rather than a claim or a read-modify-write.
func (s *Store[T]) Set(ctx context.Context, key string, v *T) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("kvstore: encode %s: %w", key, err)
	}
	return s.withKV(ctx, func(kv jetstream.KeyValue) error {
		if _, err := kv.Put(ctx, key, data); err != nil {
			return fmt.Errorf("kvstore: put %s: %w", key, err)
		}
		return nil
	})
}

// CompareAndSet writes v only if key is still at rev, returning ErrConflict
// when it is not. For callers whose lost race is a decision rather than a
// failure; callers that should retry want Mutate.
//
// The revision the write landed at is returned, for a caller chaining a second
// CAS onto its own first one.
func (s *Store[T]) CompareAndSet(ctx context.Context, key string, v *T, rev uint64) (uint64, error) {
	kv, err := s.KV(ctx)
	if err != nil {
		return 0, err
	}
	data, err := json.Marshal(v)
	if err != nil {
		return 0, fmt.Errorf("kvstore: encode %s: %w", key, err)
	}
	next, err := kv.Update(ctx, key, data, rev)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyRevisionMismatch) || errors.Is(err, jetstream.ErrKeyExists) {
			return 0, fmt.Errorf("%w: %s", ErrConflict, key)
		}
		return 0, fmt.Errorf("kvstore: update %s: %w", key, err)
	}
	return next, nil
}

// get is Get against an already-opened bucket, for loops that opened it once.
func (s *Store[T]) get(ctx context.Context, kv jetstream.KeyValue, key string) (*T, uint64, error) {
	entry, err := kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, 0, fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		return nil, 0, fmt.Errorf("kvstore: get %s: %w", key, err)
	}
	var v T
	if err := json.Unmarshal(entry.Value(), &v); err != nil {
		return nil, 0, fmt.Errorf("kvstore: decode %s: %w", key, err)
	}
	return &v, entry.Revision(), nil
}
