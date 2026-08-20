package reconcile

import (
	"context"
	"errors"
	"github.com/mulgadc/spinifex/spinifex/otelsetup"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Single CAS-elected leader key; TTL bounds crash-recovery.
const (
	KVBucketVPCDReconcile = "spinifex-vpcd-reconcile"
	reconcileLeaderKey    = "leader"

	// reconcileReleaseTimeout bounds the lock delete on the way out, which runs
	// on a detached context and so cannot inherit the caller's deadline.
	reconcileReleaseTimeout = 5 * time.Second
)

// Leader-key lifetime. Vars (not consts) so tests can shrink them.
//
// reconcileLeaderRenew is the gap between refreshes: a pass can outlive the TTL
// on its own — one stalled gw-lrp DORA runs ~64s — so the key is renewed while
// the pass runs rather than left to expire underneath it.
var (
	reconcileLeaderTTL   = 60 * time.Second
	reconcileLeaderRenew = 20 * time.Second
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
				"holder", holder, "bucket", bucket, "waited_ms", otelsetup.Millis(leaderRetryFor), "err", err)
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

	rev, err := kv.Create(ctx, reconcileLeaderKey, []byte(holder))
	if err != nil {
		slog.Info("reconcile/lock: another holder is leader, skipping reconcile", "holder", holder, "bucket", bucket, "err", err)
		return nil, false
	}

	slog.Info("reconcile/lock: elected", "holder", holder, "bucket", bucket)
	lease := &leaderLease{kv: kv, rev: rev}
	renewCtx, stopRenew := context.WithCancel(ctx)
	go lease.renew(renewCtx, holder, bucket)

	return func() {
		stopRenew()
		rev, held := lease.revision()
		if !held {
			// Renewal lost the key, so another node may already hold it. An
			// unguarded delete here would drop that node's lock, not ours.
			slog.Warn("reconcile/lock: lock already lost before release; not deleting", "holder", holder, "bucket", bucket)
			return
		}
		// Release outlives ctx: shutdown is the common reason to release, and
		// skipping the delete would park the lock for the full TTL and stall
		// every other node's reconcile.
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reconcileReleaseTimeout)
		defer cancel()
		if err := kv.Delete(releaseCtx, reconcileLeaderKey, jetstream.LastRevision(rev)); err != nil {
			slog.Warn("reconcile/lock: failed to release lock (TTL will reap)", "holder", holder, "bucket", bucket, "err", err)
		}
	}, true
}

// leaderLease tracks the revision of a held leader key. Renewal and the final
// delete are both CAS-guarded on it, so a pass that outlives the TTL can never
// delete the key of the node that took over from it.
type leaderLease struct {
	mu   sync.Mutex
	kv   jetstream.KeyValue
	rev  uint64
	lost bool
}

// revision returns the current revision and whether the lease is still held.
func (l *leaderLease) revision() (uint64, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rev, !l.lost
}

// renew refreshes the leader key until ctx is cancelled or a refresh loses the
// CAS, which means the key expired and another node claimed it.
func (l *leaderLease) renew(ctx context.Context, holder, bucket string) {
	ticker := time.NewTicker(reconcileLeaderRenew)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rev, held := l.revision()
			if !held {
				return
			}
			next, err := l.kv.Update(ctx, reconcileLeaderKey, []byte(holder), rev)
			if err != nil {
				l.mu.Lock()
				l.lost = true
				l.mu.Unlock()
				slog.Warn("reconcile/lock: leader key renewal failed; another node may take over mid-pass",
					"holder", holder, "bucket", bucket, "err", err)
				return
			}
			l.mu.Lock()
			l.rev = next
			l.mu.Unlock()
		}
	}
}
