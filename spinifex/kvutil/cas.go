package kvutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// casAttempts bounds an optimistic-concurrency loop.
const casAttempts = 5

// casBackoffBase scales the pause between attempts.
const casBackoffBase = 2 * time.Millisecond

// CASConfig tunes an optimistic-concurrency loop. The zero value retreis casAttempts times and fails when the key is absent.
type CASConfig struct {
	Attempts       int  // 0 selects casAttempts
	CreateIfAbsent bool // start from a zero value when the key is missing
}

// isConflict reports a lost CAS race. Create reports one as ErrKeyExists and Update as ErrKeyRevisionMismatch.
// A revision-guarded Delete only ever reports the latter.
func isConflict(err error) bool {
	return errors.Is(err, jetstream.ErrKeyExists) || errors.Is(err, jetstream.ErrKeyRevisionMismatch)
}

// retryCAS runs attempt until is succeeds, returns a non-conflict error, or spends its buudget. It pauses between attempts,
// jittered so contending writers do not re-collide on the same revision.
func retryCAS(ctx context.Context, cfg CASConfig, op, key string, attempt func() error) error {
	attempts := cfg.Attempts
	if attempts <= 0 {
		attempts = casAttempts
	}
	var lastErr error
	for i := range attempts {
		err := attempt()
		if err == nil {
			return nil
		}
		if !isConflict(err) {
			return err
		}
		lastErr = err
		backoff := casBackoffBase * time.Duration(i+1)
		jitter := time.Duration(rand.Int64N(int64(backoff))) //nolint:gosec // jitter, not cryptographic
		select {
		case <-time.After(backoff/2 + jitter):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fmt.Errorf("CAS %s exhausted %d attempts for key %s: %w", op, attempts, key, lastErr)
}

// Update is an optimistic-concurrency read-modify-write:
// decode the current entry, apply mutate, commit at the observed revision.
func Update[T any](ctx context.Context, kv jetstream.KeyValue, key string, cfg CASConfig, mutate func(*T) (bool, error)) (*T, error) {
	var result *T
	err := retryCAS(ctx, cfg, "update", key, func() error {
		var value T
		var revision uint64

		entry, err := kv.Get(ctx, key)
		switch {
		case err == nil:
			if uerr := json.Unmarshal(entry.Value(), &value); uerr != nil {
				return uerr
			}
			revision = entry.Revision()
		case errors.Is(err, jetstream.ErrKeyNotFound) && cfg.CreateIfAbsent:
			revision = 0
		default:
			return err
		}

		changed, merr := mutate(&value)
		if merr != nil {
			return merr
		}
		if !changed {
			result = &value
			return nil
		}

		data, merr := json.Marshal(&value)
		if merr != nil {
			return merr
		}
		if revision == 0 {
			_, err = kv.Create(ctx, key, data)
		} else {
			_, err = kv.Update(ctx, key, data, revision)
		}
		if err != nil {
			return err
		}
		result = &value
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Put is the replace-wholesale counterpart to Update, for values that cannot safely be decoded into and copied out of a shared struct.
func Put(ctx context.Context, kv jetstream.KeyValue, key string, cfg CASConfig, data []byte) error {
	return retryCAS(ctx, cfg, "put", key, func() error {
		var revision uint64
		entry, err := kv.Get(ctx, key)
		switch {
		case err == nil:
			revision = entry.Revision()
		case errors.Is(err, jetstream.ErrKeyNotFound) && cfg.CreateIfAbsent:
			revision = 0
		default:
			return err
		}
		if revision == 0 {
			_, err = kv.Create(ctx, key, data)
		} else {
			_, err = kv.Update(ctx, key, data, revision)
		}
		return err
	})
}

// Claim atomically removes key, decoding its value first. At most one caller can observe a successful delete at a given revision, so
// at most one gets a non-nil value back - the primitive an exclusive claim is built on.
func Claim[T any](ctx context.Context, kv jetstream.KeyValue, key string, cfg CASConfig) (value *T, notFound bool, err error) {
	var result *T
	var absent bool
	rerr := retryCAS(ctx, cfg, "claim", key, func() error {
		entry, gerr := kv.Get(ctx, key)
		if gerr != nil {
			if errors.Is(gerr, jetstream.ErrKeyNotFound) {
				absent = true
				return nil
			}
			return gerr
		}
		var v T
		if uerr := json.Unmarshal(entry.Value(), &v); uerr != nil {
			return uerr
		}
		if derr := kv.Delete(ctx, key, jetstream.LastRevision(entry.Revision())); derr != nil {
			return derr
		}
		result = &v
		return nil
	})
	if rerr != nil {
		return nil, false, rerr
	}
	return result, absent, nil
}
