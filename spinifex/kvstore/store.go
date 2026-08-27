package kvstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/kvutil"
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

// Delete removes a key. Idempotent: an already-absent key is success.
func (s *Store[T]) Delete(ctx context.Context, key string) error {
	err := s.withKV(ctx, func(kv jetstream.KeyValue) error {
		if err := kv.Delete(ctx, key); err != nil {
			return fmt.Errorf("kvstore: delete %s: %w", key, err)
		}
		return nil
	})
	if err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return err
	}
	return nil
}

// Purge is Delete for a key whose history must go with it, so a later Create
// sees a key that never existed rather than one with a delete marker on top.
func (s *Store[T]) Purge(ctx context.Context, key string) error {
	err := s.withKV(ctx, func(kv jetstream.KeyValue) error {
		if err := kv.Purge(ctx, key); err != nil {
			return fmt.Errorf("kvstore: purge %s: %w", key, err)
		}
		return nil
	})
	if err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return err
	}
	return nil
}

// List decodes every record whose key carries the given prefix. An empty
// prefix matches everything and an empty bucket is not an error. A key that
// disappears between the listing and the read is skipped.
func (s *Store[T]) List(ctx context.Context, prefix string) ([]T, error) {
	var out []T
	// Whole listing sits inside the wrapper, not just the key enumeration: a
	// stream lost partway through the reads has to restart from the listing,
	// so out is reset rather than appended to twice.
	err := s.withKV(ctx, func(kv jetstream.KeyValue) error {
		out = nil
		keys, err := s.keys(ctx, kv)
		if err != nil {
			return err
		}
		for _, key := range keys {
			if !strings.HasPrefix(key, prefix) {
				continue
			}
			v, _, err := s.get(ctx, kv, key)
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			out = append(out, *v)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DeletePrefix removes every key carrying the given prefix. An empty prefix
// purges the bucket. Idempotent, like Delete.
func (s *Store[T]) DeletePrefix(ctx context.Context, prefix string) error {
	// Safe to re-run: a key deleted before the stream was lost is already
	// absent on the retry, which Delete treats as success.
	return s.withKV(ctx, func(kv jetstream.KeyValue) error {
		keys, err := s.keys(ctx, kv)
		if err != nil {
			return err
		}
		for _, key := range keys {
			if !strings.HasPrefix(key, prefix) {
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

// Exists reports whether a key is present without decoding its value, so a
// record that cannot be unmarshalled is still reported as present.
func (s *Store[T]) Exists(ctx context.Context, key string) (bool, error) {
	err := s.withKV(ctx, func(kv jetstream.KeyValue) error {
		if _, err := kv.Get(ctx, key); err != nil {
			return fmt.Errorf("kvstore: get %s: %w", key, err)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// CompareAndSet writes v only if key is still at rev, returning ErrConflict
// when it is not. For callers whose lost race is a decision rather than a
// failure; callers that should retry want Mutate.
func (s *Store[T]) CompareAndSet(ctx context.Context, key string, v *T, rev uint64) error {
	kv, err := s.KV(ctx)
	if err != nil {
		return err
	}
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("kvstore: encode %s: %w", key, err)
	}
	if _, err := kv.Update(ctx, key, data, rev); err != nil {
		if errors.Is(err, jetstream.ErrKeyRevisionMismatch) || errors.Is(err, jetstream.ErrKeyExists) {
			return fmt.Errorf("%w: %s", ErrConflict, key)
		}
		return fmt.Errorf("kvstore: update %s: %w", key, err)
	}
	return nil
}

// keys lists an already-opened bucket, treating an empty bucket as an empty
// listing. Recovery is the caller's, so the raw error passes through.
func (s *Store[T]) keys(ctx context.Context, kv jetstream.KeyValue) ([]string, error) {
	keys, err := kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, err
	}
	return keys, nil
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
