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
	kv, err := s.KV(ctx)
	if err != nil {
		return nil, 0, err
	}
	return s.get(ctx, kv, key)
}

// Create writes a record only if the key is absent, returning ErrExists when
// it is not. The create-only write is the single-writer claim.
func (s *Store[T]) Create(ctx context.Context, key string, v *T) error {
	kv, err := s.KV(ctx)
	if err != nil {
		return err
	}
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("kvstore: encode %s: %w", key, err)
	}
	if _, err := kv.Create(ctx, key, data); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return fmt.Errorf("%w: %s", ErrExists, key)
		}
		return fmt.Errorf("kvstore: create %s: %w", key, err)
	}
	return nil
}

// Mutate applies fn under a revision-guarded retry loop. fn reports whether it
// changed anything; false commits no write. An absent key is ErrNotFound.
func (s *Store[T]) Mutate(ctx context.Context, key string, fn func(*T) (bool, error)) error {
	kv, err := s.KV(ctx)
	if err != nil {
		return err
	}
	_, err = kvutil.Update(ctx, kv, key, kvutil.CASConfig{
		Attempts:  s.cfg.Attempts,
		NotFound:  fmt.Errorf("%w: %s", ErrNotFound, key),
		Exhausted: s.cfg.Exhausted,
	}, fn)
	return err
}

// Delete removes a key. Idempotent: an already-absent key is success.
func (s *Store[T]) Delete(ctx context.Context, key string) error {
	kv, err := s.KV(ctx)
	if err != nil {
		return err
	}
	if err := kv.Delete(ctx, key); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return fmt.Errorf("kvstore: delete %s: %w", key, err)
	}
	return nil
}

// List decodes every record whose key carries the given prefix. An empty
// prefix matches everything and an empty bucket is not an error. A key that
// disappears between the listing and the read is skipped.
func (s *Store[T]) List(ctx context.Context, prefix string) ([]T, error) {
	kv, keys, err := s.keys(ctx)
	if err != nil {
		return nil, err
	}
	var out []T
	for _, key := range keys {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		v, _, err := s.get(ctx, kv, key)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, nil
}

// DeletePrefix removes every key carrying the given prefix. An empty prefix
// purges the bucket. Idempotent, like Delete.
func (s *Store[T]) DeletePrefix(ctx context.Context, prefix string) error {
	kv, keys, err := s.keys(ctx)
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
}

// Over returns a Store for records of type T over an already-open bucket.
func Over[T any](kv jetstream.KeyValue, cfg Config) *Store[T] {
	return &Store[T]{Bucket: NewOpenBucket(kv, cfg)}
}

// Set writes a record whether or not the key exists, for callers whose write
// is a replacement rather than a claim or a read-modify-write.
func (s *Store[T]) Set(ctx context.Context, key string, v *T) error {
	kv, err := s.KV(ctx)
	if err != nil {
		return err
	}
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("kvstore: encode %s: %w", key, err)
	}
	if _, err := kv.Put(ctx, key, data); err != nil {
		return fmt.Errorf("kvstore: put %s: %w", key, err)
	}
	return nil
}

// Exists reports whether a key is present without decoding its value, so a
// record that cannot be unmarshalled is still reported as present.
func (s *Store[T]) Exists(ctx context.Context, key string) (bool, error) {
	kv, err := s.KV(ctx)
	if err != nil {
		return false, err
	}
	if _, err := kv.Get(ctx, key); err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("kvstore: get %s: %w", key, err)
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

// keys lists the bucket, treating an empty bucket as an empty listing.
func (s *Store[T]) keys(ctx context.Context) (jetstream.KeyValue, []string, error) {
	kv, err := s.KV(ctx)
	if err != nil {
		return nil, nil, err
	}
	keys, err := kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return kv, nil, nil
		}
		return nil, nil, fmt.Errorf("kvstore: list keys: %w", err)
	}
	return kv, keys, nil
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
