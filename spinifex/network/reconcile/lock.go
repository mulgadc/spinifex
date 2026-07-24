package reconcile

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Single CAS-elected leader key; TTL bounds crash-recovery.
const (
	KVBucketVPCDReconcile = "spinifex-vpcd-reconcile"
	reconcileLeaderKey    = "leader"
	reconcileLeaderTTL    = 60 * time.Second

	// reconcileReleaseTimeout bounds the lock delete on the way out, which runs
	// on a detached context and so cannot inherit the caller's deadline.
	reconcileReleaseTimeout = 5 * time.Second
)

// Bounded wait for JetStream quorum on cold multi-node start. Vars (not
// consts) so tests can shrink them.
var (
	leaderRetryFor  = 60 * time.Second
	leaderRetryStep = 1 * time.Second
)

// AcquireLeader elects one leader on the named lock bucket. Independent
// reconcile loops pass distinct buckets so they never share a single mutex: the
// gateway quota reconcile must not block vpcd's network reconcile, and vice
// versa.
func AcquireLeader(ctx context.Context, nc *nats.Conn, bucket, holder string) (func(), bool) {
	js, err := jetstream.New(nc)
	if err != nil {
		slog.Error("reconcile/lock: JetStream unavailable, skipping reconcile",
			"holder", holder, "bucket", bucket, "err", err)
		return nil, false
	}

	var kv jetstream.KeyValue
	deadline := time.Now().Add(leaderRetryFor)
	for {
		// Get-or-create: CreateKeyValue returns "stream name already in use" if
		// the bucket exists; attach first, create only when genuinely absent.
		kv, err = js.KeyValue(ctx, bucket)
		if errors.Is(err, jetstream.ErrBucketNotFound) {
			kv, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
				Bucket:  bucket,
				History: 1,
				TTL:     reconcileLeaderTTL,
			})
		}
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			slog.Error("reconcile/lock: JetStream KV unreachable after retry, skipping reconcile",
				"holder", holder, "bucket", bucket, "waited", leaderRetryFor, "err", err)
			return nil, false
		}
		slog.Debug("reconcile/lock: JetStream KV not ready, retrying", "holder", holder, "bucket", bucket, "err", err)
		// A shutdown mid-wait must not sit out the remaining retry window.
		select {
		case <-ctx.Done():
			slog.Info("reconcile/lock: cancelled while waiting for JetStream KV", "holder", holder, "bucket", bucket)
			return nil, false
		case <-time.After(leaderRetryStep):
		}
	}

	if _, err := kv.Create(ctx, reconcileLeaderKey, []byte(holder)); err != nil {
		slog.Info("reconcile/lock: another holder is leader, skipping reconcile", "holder", holder, "bucket", bucket, "err", err)
		return nil, false
	}

	slog.Info("reconcile/lock: elected", "holder", holder, "bucket", bucket)
	return func() {
		// Release outlives ctx: shutdown is the common reason to release, and
		// skipping the delete would park the lock for the full TTL and stall
		// every other node's reconcile.
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reconcileReleaseTimeout)
		defer cancel()
		if err := kv.Delete(releaseCtx, reconcileLeaderKey); err != nil {
			slog.Warn("reconcile/lock: failed to release lock (TTL will reap)", "holder", holder, "bucket", bucket, "err", err)
		}
	}, true
}
