package reconcile

import (
	"context"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/kvlease"
	"github.com/nats-io/nats.go"
)

// Single CAS-elected leader key; TTL bounds crash-recovery.
const (
	KVBucketVPCDReconcile = "spinifex-vpcd-reconcile"
	reconcileLeaderKey    = "leader"
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

// AcquireLeader elects one leader on the named lock bucket. Independent
// reconcile loops pass distinct buckets so they never share a single mutex: the
// gateway quota reconcile must not block vpcd's network reconcile, and vice
// versa.
func AcquireLeader(ctx context.Context, nc *nats.Conn, bucket, holder string) (func(), bool) {
	lease, err := kvlease.New(kvlease.Config{
		Name:   "reconcile/lock",
		Bucket: kvlease.NATSBucket(nc, bucket, reconcileLeaderTTL),
		Key:    reconcileLeaderKey,
		Holder: holder,
		TTL:    reconcileLeaderTTL,
		Renew:  reconcileLeaderRenew,
	})
	if err != nil {
		slog.ErrorContext(ctx, "reconcile/lock: lease config invalid",
			"holder", holder, "bucket", bucket, "err", err)
		return nil, false
	}
	if !lease.TryAcquire(ctx) {
		slog.InfoContext(ctx, "reconcile/lock: not leader, skipping reconcile",
			"holder", holder, "bucket", bucket)
		return nil, false
	}
	return func() { lease.Release(ctx) }, true
}
