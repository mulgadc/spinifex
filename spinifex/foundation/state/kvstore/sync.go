package kvstore

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvsync"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

var _ kvsync.KVStore = (*Bucket)(nil)

// syncBucket holds every kvsync lock key, namespaced by the key itself.
const (
	syncBucket        = "spinifex-sync"
	syncBucketVersion = 1
)

// OpenSyncBucket returns the Bucket every kvsync lock in a process is built on:
// file storage, one revision of history and no TTL, since a lease's expiry is
// observed by its contenders rather than kept by the store. It opens on first use.
//
// RecreateIfMissing stays false: a recreated stream restarts its revisions, and
// a generation must only grow.
func OpenSyncBucket(js jetstream.JetStream) *Bucket {
	return NewBucket(js, Config{
		Name:        syncBucket,
		History:     1,
		Description: "kvsync claims and leases",
		Missing:     "kvstore: sync bucket has no JetStream client",
		OnOpen: func(ctx context.Context, kv jetstream.KeyValue) error {
			return WriteVersion(ctx, kv, syncBucketVersion)
		},
	})
}

// Load returns the value and revision of key, with an error that is both
// ErrNotFound and kvsync.ErrNotFound when the key is absent.
func (b *Bucket) Load(ctx context.Context, key string) ([]byte, uint64, error) {
	var (
		value []byte
		rev   uint64
	)
	err := b.withKV(ctx, func(kv jetstream.KeyValue) error {
		entry, err := kv.Get(ctx, key)
		if err != nil {
			return err
		}
		value, rev = entry.Value(), entry.Revision()
		return nil
	})
	if err != nil {
		return nil, 0, syncError("load", key, err)
	}
	return value, rev, nil
}

// CompareAndSwap writes value only while key is at rev, where rev 0 means the
// key must be absent or deleted, and returns the new revision. A lost race is
// both ErrConflict and kvsync.ErrRevisionMismatch.
func (b *Bucket) CompareAndSwap(ctx context.Context, key string, value []byte, rev uint64) (uint64, error) {
	kv, err := b.KV(ctx)
	if err != nil {
		return 0, syncError("compare-and-swap", key, err)
	}
	// Not through withKV: the revision is the caller's, as in CompareAndDelete.
	var next uint64
	if rev == 0 {
		next, err = kv.Create(ctx, key, value)
	} else {
		next, err = kv.Update(ctx, key, value, rev)
	}
	if err != nil {
		return 0, syncError("compare-and-swap", key, err)
	}
	return next, nil
}

// syncError classifies a JetStream failure into both packages' sentinels.
func syncError(op, key string, err error) error {
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return fmt.Errorf("%w: %w: %s", ErrNotFound, kvsync.ErrNotFound, key)
	case IsRevisionConflict(err):
		return fmt.Errorf("%w: %w: %s", ErrConflict, kvsync.ErrRevisionMismatch, key)
	case isUnreachable(err):
		return fmt.Errorf("kvstore: %s %s: %w: %w", op, key, kvsync.ErrUnavailable, err)
	default:
		return fmt.Errorf("kvstore: %s %s: %w", op, key, err)
	}
}

// isUnreachable is IsStreamUnavailable widened to a connection that is down, a
// request that went unanswered, or a server answering 503 (10008 while JetStream
// has no meta leader), since a lock caller retries all alike.
func isUnreachable(err error) bool {
	if apiErr, ok := errors.AsType[*jetstream.APIError](err); ok && apiErr.Code == http.StatusServiceUnavailable {
		return true
	}
	return IsStreamUnavailable(err) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, nats.ErrTimeout) ||
		errors.Is(err, nats.ErrConnectionClosed) ||
		errors.Is(err, nats.ErrConnectionReconnecting) ||
		errors.Is(err, nats.ErrNoServers) ||
		errors.Is(err, nats.ErrDisconnected)
}
