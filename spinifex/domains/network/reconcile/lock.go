package reconcile

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvsync"
)

// KeyVPCDReconcile is vpcd's network reconcile lease in the sync bucket.
const KeyVPCDReconcile = "vpcd.reconcile"

// LeaderTTL is the per-pass leader leases' time to live. A pass can outlive it
// on its own (one stalled gw-lrp DORA runs ~64s), so the lease renews itself
// while the pass runs rather than lapsing underneath it.
const LeaderTTL = 60 * time.Second

// NewLeaderLease returns a per-pass leader lease on key. Each is built once per
// process and shared by every site that reconciles under it, so those sites
// exclude each other on one node as well as across nodes.
func NewLeaderLease(store kvsync.KVStore, key, holder string) *kvsync.Lease {
	return kvsync.NewLease(store, key, holder, LeaderTTL)
}

// NewVPCDLease returns vpcd's network reconcile lease.
func NewVPCDLease(store kvsync.KVStore, holder string) *kvsync.Lease {
	return NewLeaderLease(store, KeyVPCDReconcile, holder)
}

// AcquireLeader makes one attempt to take lease for a pass. Independent
// reconcile loops hold distinct leases, so the gateway quota reconcile never
// blocks vpcd's network reconcile. The release survives a cancelled ctx.
func AcquireLeader(ctx context.Context, lease *kvsync.Lease) (func(), bool) {
	tok, err := lease.TryLock(ctx)
	if err != nil {
		if held, ok := errors.AsType[*kvsync.HeldError](err); ok {
			slog.InfoContext(ctx, "reconcile/lock: not leader, skipping reconcile",
				"key", lease.Key(), "holder", held.Current.Holder)
		} else {
			slog.WarnContext(ctx, "reconcile/lock: election failed, skipping reconcile",
				"key", lease.Key(), "err", err)
		}
		return nil, false
	}
	return func() { lease.Unlock(ctx, tok) }, true
}
