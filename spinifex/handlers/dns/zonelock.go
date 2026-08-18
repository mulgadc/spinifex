package dns

import (
	"context"
	"errors"
	"fmt"
	"github.com/mulgadc/spinifex/spinifex/otelsetup"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// KVBucketDNSZoneLock is dedicated to zone write locks: a zone held mid-write
	// must never block an unrelated reconcile loop sharing a bucket.
	KVBucketDNSZoneLock = "spinifex-dns-zone-lock"

	// zoneLockTTL bounds crash recovery — a holder that dies mid-write parks its
	// zone for at most this long. Deliberately above requestTimeout so a producer
	// has already abandoned its request before the lock it is waiting on can
	// expire under a still-running holder.
	zoneLockTTL = 10 * time.Second

	// zoneLockBucketTimeout bounds attaching to the bucket. Kept short because a
	// producer is blocked on requestTimeout throughout; JetStream being unready is
	// better reported than waited out.
	zoneLockBucketTimeout = 2 * time.Second
)

// Acquire blocks with backoff rather than skipping like a reconcile leader
// election: a writer must apply its change. The total wait stays under
// requestTimeout so the producer's ack is not lost to lock contention. Vars, not
// consts, so tests can shrink them.
var (
	zoneLockWaitFor = 2500 * time.Millisecond
	zoneLockStep    = 20 * time.Millisecond
	zoneLockStepMax = 200 * time.Millisecond
)

// zoneLocker hands out cluster-wide per-zone write locks over JetStream KV. It
// exists because a NATS queue group load-balances messages, it does not
// serialise them: every daemon in the queue group can otherwise read-modify-write
// the same zone object concurrently, losing records or corrupting the TOML.
type zoneLocker struct {
	nc     *nats.Conn
	holder string

	mu sync.Mutex
	kv jetstream.KeyValue
}

// newZoneLocker builds a locker for one node. A nil conn yields a locker that
// no-ops, which is correct only because a writer without a NATS connection has no
// queue-group peer to race — bindConn upgrades it if a connection appears.
func newZoneLocker(nc *nats.Conn, holder string) *zoneLocker {
	if strings.TrimSpace(holder) == "" {
		holder = "unknown"
	}
	return &zoneLocker{nc: nc, holder: holder}
}

// bindConn adopts a connection when the locker was built without one, so a
// writer that only receives its conn at Subscribe time still locks.
func (l *zoneLocker) bindConn(nc *nats.Conn) {
	if l == nil || nc == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.nc == nil {
		l.nc = nc
	}
}

// zoneLock is a held per-zone lock. Callers must Release it, and should check
// Expired before a write that would land outside the lease.
type zoneLock struct {
	locker   *zoneLocker
	kv       jetstream.KeyValue
	key      string
	zone     string
	acquired time.Time
}

// lockZone acquires the cluster-wide write lock for one zone, blocking until it
// is free or the wait budget is spent.
func (l *zoneLocker) lockZone(ctx context.Context, zone string) (*zoneLock, error) {
	if l == nil {
		return nil, nil
	}
	l.mu.Lock()
	nc := l.nc
	l.mu.Unlock()
	if nc == nil {
		// No connection means no queue-group peer, so no cluster-wide race exists
		// to guard against. Covers direct in-process ApplyBatch callers and tests.
		return nil, nil
	}

	kv, err := l.bucket(ctx)
	if err != nil {
		return nil, err
	}

	key := zoneLockKey(zone)
	deadline := time.Now().Add(zoneLockWaitFor)
	step := zoneLockStep
	for {
		if _, err := kv.Create(ctx, key, []byte(l.holder)); err == nil {
			return &zoneLock{locker: l, kv: kv, key: key, zone: zone, acquired: time.Now()}, nil
		} else if !isZoneLockHeld(err) {
			return nil, fmt.Errorf("acquire dns zone lock %s: %w", zone, err)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("acquire dns zone lock %s: contended for %s", zone, zoneLockWaitFor)
		}
		// Jitter so several blocked writers do not retry in lockstep and starve
		// each other on the same millisecond.
		wait := step + rand.N(step) //nolint:gosec // jitter, not cryptographic
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("acquire dns zone lock %s: %w", zone, ctx.Err())
		case <-time.After(wait):
		}
		step = min(step*2, zoneLockStepMax)
	}
}

// Expired reports whether the lease may already have been reaped, meaning
// another writer could now hold the zone. A caller that has not yet written must
// abort rather than write unserialised.
func (z *zoneLock) Expired() bool {
	if z == nil {
		return false
	}
	return time.Since(z.acquired) >= zoneLockTTL
}

// Release frees the zone. It runs on a detached context because the common
// caller is a deferred cleanup whose own context may already be done, and
// skipping the delete would park the zone for the full TTL.
func (z *zoneLock) Release(ctx context.Context) {
	if z == nil {
		return
	}
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), zoneLockBucketTimeout)
	defer cancel()
	if err := z.kv.Delete(releaseCtx, z.key); err != nil {
		slog.Warn("dns writer: failed to release zone lock (TTL will reap)",
			"zone", z.zone, "holder", z.locker.holder, "ttl_ms", otelsetup.Millis(zoneLockTTL), "error", err)
	}
}

// bucket attaches to the lock bucket, creating it when absent, and caches the
// handle. Lazy because the writer is constructed before JetStream is guaranteed
// ready.
func (l *zoneLocker) bucket(ctx context.Context) (jetstream.KeyValue, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.kv != nil {
		return l.kv, nil
	}

	js, err := jetstream.New(l.nc)
	if err != nil {
		return nil, fmt.Errorf("dns zone lock: JetStream unavailable: %w", err)
	}

	attachCtx, cancel := context.WithTimeout(ctx, zoneLockBucketTimeout)
	defer cancel()

	// Get-or-create: CreateKeyValue reports the stream name as in use when the
	// bucket exists, so attach first and create only when genuinely absent.
	kv, err := js.KeyValue(attachCtx, KVBucketDNSZoneLock)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		kv, err = js.CreateKeyValue(attachCtx, jetstream.KeyValueConfig{
			Bucket:  KVBucketDNSZoneLock,
			History: 1,
			TTL:     zoneLockTTL,
		})
		// Another node created it in the gap; attach to theirs.
		if err != nil {
			kv, err = js.KeyValue(attachCtx, KVBucketDNSZoneLock)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("dns zone lock: KV bucket %s unavailable: %w", KVBucketDNSZoneLock, err)
	}

	l.kv = kv
	return kv, nil
}

// isZoneLockHeld reports whether the create failed because another writer holds
// the zone, which is the retryable case. Mirrors the CAS-conflict test used by
// the vCPU quota store.
func isZoneLockHeld(err error) bool {
	if errors.Is(err, jetstream.ErrKeyExists) {
		return true
	}
	var apiErr *jetstream.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode == jetstream.JSErrCodeStreamWrongLastSequence
	}
	return false
}

// zoneLockKey maps a zone to a NATS KV key. DNS names are already within the
// legal key charset, so this only normalises case and guards odd input rather
// than encoding anything.
func zoneLockKey(zone string) string {
	z := strings.ToLower(strings.Trim(strings.TrimSpace(zone), "."))
	if z == "" {
		return "_apex"
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
			return r
		default:
			return '_'
		}
	}, z)
}
