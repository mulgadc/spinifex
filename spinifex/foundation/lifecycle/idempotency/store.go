// Package idempotency implements request idempotency over a TTL KV bucket: the
// first caller to present a token owns the work, and duplicates replay its
// result or wait for it. The payload a replay returns is the caller's own type.
package idempotency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

const (
	statusInFlight = "in-flight"
	statusDone     = "done"
)

// waitTimeout caps how long a duplicate caller polls an in-flight owner.
// pollStep is the inter-poll sleep. Vars so tests can shrink them.
var (
	waitTimeout = 30 * time.Second
	pollStep    = 250 * time.Millisecond
)

// ErrParamMismatch signals a token reused with different request parameters: a
// caller error, and the only token failure that is not ErrUnavailable.
var ErrParamMismatch = errors.New("idempotency: parameter mismatch")

// ErrUnavailable signals the token record could not be read or settled, so the
// request cannot be safely deduplicated. Every store failure wraps it, which is
// what lets a caller map them all onto one API error.
var (
	ErrUnavailable = errors.New("idempotency: token record unavailable")
	ErrWaitTimeout = fmt.Errorf("%w: timed out waiting for the in-flight owner", ErrUnavailable)
)

// record is the per-token record stored in the KV bucket.
type record[T any] struct {
	Status    string    `json:"status"`
	ParamHash string    `json:"paramHash"`
	StartedAt time.Time `json:"startedAt"`
	Payload   *T        `json:"payload,omitempty"`
}

// Store owns the token records for one kind of request. T is what a duplicate
// caller replays: a whole response, or just the name of what was created.
type Store[T any] struct {
	kv        jetstream.KeyValue
	namespace string
}

// OpenBucket binds the bucket, creating it with the given TTL if absent. Split
// from NewStore so several typed stores can share one bucket without rebinding
// it per request.
func OpenBucket(ctx context.Context, js jetstream.JetStream, bucket string, ttl time.Duration) (jetstream.KeyValue, error) {
	kv, err := js.KeyValue(ctx, bucket)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		kv, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
			Bucket:  bucket,
			History: 1,
			TTL:     ttl,
		})
	}
	if err != nil {
		return nil, fmt.Errorf("open idempotency bucket %s: %w", bucket, err)
	}
	return kv, nil
}

// NewStore builds a typed view over an already-bound bucket. Stores sharing a
// bucket must use distinct namespaces, else one caller's record is decoded as
// another caller's T.
func NewStore[T any](kv jetstream.KeyValue, namespace string) *Store[T] {
	return &Store[T]{kv: kv, namespace: namespace}
}

// OpenStore binds the bucket and takes the whole of it, with unprefixed keys.
// The caller owns the JetStream handle, so a store never outlives the
// connection it was built from.
func OpenStore[T any](ctx context.Context, js jetstream.JetStream, bucket string, ttl time.Duration) (*Store[T], error) {
	kv, err := OpenBucket(ctx, js, bucket, ttl)
	if err != nil {
		return nil, err
	}
	return NewStore[T](kv, ""), nil
}

// Key scopes a token to its account, so the same token from two accounts is two
// separate pieces of work.
func Key(accountID, token string) string {
	return accountID + "." + token
}

// key prefixes Key with the store's namespace. An empty namespace yields the
// bare account.token key, which is what the stores that predate namespacing
// have on disk.
func (s *Store[T]) key(accountID, token string) string {
	if s.namespace == "" {
		return Key(accountID, token)
	}
	return s.namespace + "." + Key(accountID, token)
}

// ParamHash hashes a request so a token reused with different parameters is
// caught. The caller must clear the token field first, so identical requests
// always hash identically.
func ParamHash(request any) string {
	b, err := json.Marshal(request)
	if err != nil {
		// Fallback: idempotency degrades to token-only.
		b = []byte{}
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Claim attempts to own the work for this token. It returns (nil, true, nil)
// when the caller owns it and must Finalize or Abort, (payload, false, nil) to
// replay a completed one, or an error on mismatch or timeout.
func (s *Store[T]) Claim(ctx context.Context, accountID, token, paramHash string) (*T, bool, error) {
	key := s.key(accountID, token)
	inflight, err := json.Marshal(record[T]{
		Status:    statusInFlight,
		ParamHash: paramHash,
		StartedAt: time.Now().UTC(),
	})
	if err != nil {
		return nil, false, fmt.Errorf("%w: marshal: %w", ErrUnavailable, err)
	}
	if _, cerr := s.kv.Create(ctx, key, inflight); cerr == nil {
		return nil, true, nil
	} else if !errors.Is(cerr, jetstream.ErrKeyExists) {
		return nil, false, fmt.Errorf("%w: create %s: %w", ErrUnavailable, key, cerr)
	}

	deadline := time.Now().Add(waitTimeout)
	for {
		entry, gerr := s.kv.Get(ctx, key)
		if gerr != nil {
			if errors.Is(gerr, jetstream.ErrKeyNotFound) {
				// Owner aborted or the record aged out: race for ownership.
				if _, rcerr := s.kv.Create(ctx, key, inflight); rcerr == nil {
					return nil, true, nil
				} else if !errors.Is(rcerr, jetstream.ErrKeyExists) {
					return nil, false, fmt.Errorf("%w: recreate %s: %w", ErrUnavailable, key, rcerr)
				}
				continue
			}
			return nil, false, fmt.Errorf("%w: get %s: %w", ErrUnavailable, key, gerr)
		}
		var rec record[T]
		if uerr := json.Unmarshal(entry.Value(), &rec); uerr != nil {
			return nil, false, fmt.Errorf("%w: unmarshal %s: %w", ErrUnavailable, key, uerr)
		}
		if rec.ParamHash != paramHash {
			return nil, false, ErrParamMismatch
		}
		if rec.Status == statusDone {
			return rec.Payload, false, nil
		}
		if time.Now().After(deadline) {
			return nil, false, ErrWaitTimeout
		}
		time.Sleep(pollStep)
	}
}

// Finalize marks the token done with the payload duplicates should replay.
func (s *Store[T]) Finalize(ctx context.Context, accountID, token, paramHash string, payload T) error {
	key := s.key(accountID, token)
	data, err := json.Marshal(record[T]{
		Status:    statusDone,
		ParamHash: paramHash,
		StartedAt: time.Now().UTC(),
		Payload:   &payload,
	})
	if err != nil {
		return fmt.Errorf("idempotency finalize marshal: %w", err)
	}
	if _, err := s.kv.Put(ctx, key, data); err != nil {
		return fmt.Errorf("idempotency finalize put %s: %w", key, err)
	}
	return nil
}

// Abort drops the in-flight token so a retry can re-attempt the work.
func (s *Store[T]) Abort(ctx context.Context, accountID, token string) {
	key := s.key(accountID, token)
	if err := s.kv.Delete(ctx, key); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		slog.Warn("idempotency: failed to abort token record", "key", key, "err", err)
	}
}
