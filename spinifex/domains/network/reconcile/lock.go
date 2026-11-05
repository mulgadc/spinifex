package reconcile

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/clustersize"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvsync"
	otelsetup "github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
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

// takeoverRetry is when a pass that found the lease held asks to run again. A
// dead holder's key is taken over once a follower has seen its revision sit for
// a TTL, so retrying at the renewal interval takes over within about 4*TTL/3.
const takeoverRetry = LeaderTTL / 3

// Bounded wait for JetStream quorum on a cold multi-node start, for a one-shot
// election that has no later pass to retry it. Vars so tests can shrink them.
var (
	startRetryFor  = 60 * time.Second
	startRetryStep = time.Second
)

// ElectPass is AcquireLeader for a pass in a loop, adding a revisit deadline:
// a pass that found the lease held asks to run again once a dead holder could
// have been waited out, rather than waiting for the loop's next resync.
func ElectPass(ctx context.Context, lease *kvsync.Lease) (release func(), revisit time.Duration, ok bool) {
	release, err := tryLeader(ctx, lease)
	if err != nil {
		if _, held := errors.AsType[*kvsync.HeldError](err); held {
			return nil, takeoverRetry, false
		}
		return nil, 0, false
	}
	return release, 0, true
}

// AcquireLeaderAtStart is AcquireLeader for a one-shot election at start,
// retrying until JetStream has formed quorum or the wait runs out, since a cold
// cluster fails in many shapes. A held lease or a config fault answers at once.
func AcquireLeaderAtStart(ctx context.Context, lease *kvsync.Lease) (func(), bool) {
	deadline := time.Now().Add(startRetryFor)
	for {
		tok, err := lease.TryLock(ctx)
		if err == nil {
			return releaser(ctx, lease, tok), true
		}
		if _, held := errors.AsType[*kvsync.HeldError](err); held || clustersize.Permanent(err) {
			logFailure(ctx, lease, err)
			return nil, false
		}
		if !time.Now().Before(deadline) {
			slog.WarnContext(ctx, "reconcile/lock: election failed until the start wait ran out, skipping reconcile",
				"key", lease.Key(), "wait_ms", otelsetup.Millis(startRetryFor), "err", err)
			return nil, false
		}
		slog.DebugContext(ctx, "reconcile/lock: election failed at start, retrying", "key", lease.Key(), "err", err)
		select {
		case <-ctx.Done():
			return nil, false
		case <-time.After(startRetryStep):
		}
	}
}

// AcquireLeader makes one attempt to take lease for a pass. Independent
// reconcile loops hold distinct leases, so the gateway quota reconcile never
// blocks vpcd's network reconcile. The release survives a cancelled ctx.
func AcquireLeader(ctx context.Context, lease *kvsync.Lease) (func(), bool) {
	release, err := tryLeader(ctx, lease)
	return release, err == nil
}

// tryLeader makes one attempt and logs a failure. The release survives a
// cancelled ctx.
func tryLeader(ctx context.Context, lease *kvsync.Lease) (func(), error) {
	tok, err := lease.TryLock(ctx)
	if err != nil {
		logFailure(ctx, lease, err)
		return nil, err
	}
	return releaser(ctx, lease, tok), nil
}

func releaser(ctx context.Context, lease *kvsync.Lease, tok kvsync.Token) func() {
	return func() { lease.Unlock(ctx, tok) }
}

func logFailure(ctx context.Context, lease *kvsync.Lease, err error) {
	if held, ok := errors.AsType[*kvsync.HeldError](err); ok {
		slog.InfoContext(ctx, "reconcile/lock: not leader, skipping reconcile",
			"key", lease.Key(), "holder", held.Current.Holder)
		return
	}
	slog.WarnContext(ctx, "reconcile/lock: election failed, skipping reconcile",
		"key", lease.Key(), "err", err)
}
