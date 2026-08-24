// Package kvlease elects a single holder of a JetStream KV key. The key's TTL
// bounds crash recovery; background renewal keeps it alive while the holder
// works, so a pass that outlives the TTL does not lose the lease underneath it.
package kvlease

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// releaseTimeout bounds the delete on the way out, which runs on a detached
// context and so cannot inherit the caller's deadline.
const releaseTimeout = 5 * time.Second

var errNotHolder = errors.New("kvlease: another node holds the lease")

// BucketFunc returns the KV bucket holding the lease key. It is injected because
// each subsystem intialises its own bucket, some with migrations attached.
type BucketFunc func(context.Context) (jetstream.KeyValue, error)

// Config describes a single lease. Name appears in logs; Retry applies only to Run.
type Config struct {
	Name   string
	Bucket BucketFunc
	Key    string
	Holder string

	TTL   time.Duration // must match the bucket's TTL
	Renew time.Duration // gap between refreshes
	Retry time.Duration // session mode only: non-leader re-attempt interval

	// Edges fire per transition, never under the lock
	OnGained func(context.Context) error
	OnLost   func()
}

// Lease is a CAS-guarded claim on a single key. The zero value is unusable; call New.
type Lease struct {
	cfg  Config
	mu   sync.Mutex
	kv   jetstream.KeyValue
	rev  uint64
	held bool
	stop context.CancelFunc
}

// New validates cfg and returns an unclaimed lease. Renew must leave room for a failed refresh,
// so it may be at most half the TTL.
func New(cfg Config) (*Lease, error) {
	switch {
	case cfg.Bucket == nil:
		return nil, errors.New("kvlease: Bucket is required")
	case cfg.Key == "":
		return nil, errors.New("kvlease: Key is required")
	case cfg.Holder == "":
		return nil, errors.New("kvlease: Holder is required")
	case cfg.TTL <= 0:
		return nil, errors.New("kvlease: TTL must be positive")
	case cfg.Renew <= 0 || cfg.Renew*2 > cfg.TTL:
		return nil, fmt.Errorf("kvlease: Renew must be in (0, TTL/2]; got renew=%s ttl=%s", cfg.Renew, cfg.TTL)
	}
	if cfg.Name == "" {
		cfg.Name = cfg.Key
	}
	if cfg.Retry <= 0 {
		cfg.Retry = cfg.Renew
	}
	return &Lease{cfg: cfg}, nil
}

// Held reports whether this node currently holds the lease.
func (l *Lease) Held() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.held
}

// TryAcquire makes one attempt to claim the key, starting background renewal and firing OnGained on
// success. A node that already holds the lease is reported as still holding it without touching the key.
func (l *Lease) TryAcquire(ctx context.Context) bool {
	if l.Held() {
		return true
	}
	kv, err := l.cfg.Bucket(ctx)
	if err != nil {
		slog.WarnContext(ctx, "kvlease: bucket unavailable",
			"lease", l.cfg.Name, "holder", l.cfg.Holder, "err", err)
		return false
	}
	rev, err := l.claim(ctx, kv)
	if err != nil {
		return false
	}

	renewCtx, stop := context.WithCancel(ctx)
	l.mu.Lock()
	if l.stop != nil {
		l.stop()
	}
	l.kv, l.rev, l.held, l.stop = kv, rev, true, stop
	l.mu.Unlock()
	go l.renew(renewCtx)

	slog.InfoContext(ctx, "kvlease: elected", "lease", l.cfg.Name, "holder", l.cfg.Holder)
	if l.cfg.OnGained != nil {
		if err := l.cfg.OnGained(ctx); err != nil {
			// A leader that cannot set up its work is work is worse than no leader. Stand down so a healthy node wins.
			slog.ErrorContext(ctx, "kvlease: OnGained failed, standing down",
				"lease", l.cfg.Name, "holder", l.cfg.Holder, "err", err)
			l.Release(ctx)
			return false
		}
	}
	return true
}

// claim creates the key or adopts one this holder left behind. A restarted
// process finds its own key still present and must refresh it rather than
// sit out the TTL with no leader elected.
func (l *Lease) claim(ctx context.Context, kv jetstream.KeyValue) (uint64, error) {
	rev, err := kv.Create(ctx, l.cfg.Key, []byte(l.cfg.Holder))
	if err == nil {
		return rev, nil
	}
	entry, gerr := kv.Get(ctx, l.cfg.Key)
	if gerr != nil {
		return 0, gerr
	}
	if string(entry.Value()) != l.cfg.Holder {
		return 0, errNotHolder
	}
	return kv.Update(ctx, l.cfg.Key, []byte(l.cfg.Holder), entry.Revision())
}

// renew refreshes the key at the revision last observed, until ctx is cancelled or a refresh
// loses the CAS - which means the key expired and another node claimed it.
func (l *Lease) renew(ctx context.Context) {
	ticker := time.NewTicker(l.cfg.Renew)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.mu.Lock()
			kv, rev, held := l.kv, l.rev, l.held
			l.mu.Unlock()
			if !held {
				return
			}
			next, err := kv.Update(ctx, l.cfg.Key, []byte(l.cfg.Holder), rev)
			if err != nil {
				slog.WarnContext(ctx, "kvlease: renewal lost the key, another node may take over mid-pass",
					"lease", l.cfg.Name, "holder", l.cfg.Holder, "err", err)
				l.markLost()
				return
			}
			l.mu.Lock()
			l.rev = next
			l.mu.Unlock()
		}
	}
}

// markLost clears held state and fires OnLost. It reports whether this call was the transition,
// so renewal and release never double-fire the edge.
func (l *Lease) markLost() bool {
	l.mu.Lock()
	was := l.held
	l.held = false
	l.mu.Unlock()
	if !was {
		return false
	}
	if l.cfg.OnLost != nil {
		l.cfg.OnLost()
	}
	return true
}

// Release deletes the key if this node still owns it at the revision it last observed.
// It runs on a detached context: shutdown cancels ctx first, and skipping the delete would park
// the lease for the full TTL.
func (l *Lease) Release(ctx context.Context) {
	l.mu.Lock()
	kv, rev, stop := l.kv, l.rev, l.stop
	l.stop = nil
	l.mu.Unlock()
	if stop != nil {
		stop()
	}
	if !l.markLost() {
		// Renewal already lost the key, so another node may hold it now. An unguarded delete here
		// would drop that node's lease, not ours.
		slog.WarnContext(ctx, "kvlease: already lost before release, not deleting",
			"lease", l.cfg.Name, "holder", l.cfg.Holder)
		return
	}
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	if err := kv.Delete(releaseCtx, l.cfg.Key, jetstream.LastRevision(rev)); err != nil {
		slog.WarnContext(releaseCtx, "kvlease: release failed, TTL will reap",
			"lease", l.cfg.Name, "holder", l.cfg.Holder, "err", err)
	}
}

// Run holds the lease across many passes, re-attempting on a ticker while this node is not the
// holder. A holder does not re-attempt: renewal keeps the key.
func (l *Lease) Run(ctx context.Context) {
	ticker := time.NewTicker(l.cfg.Retry)
	defer ticker.Stop()
	l.TryAcquire(ctx)
	for {
		select {
		case <-ctx.Done():
			l.Release(ctx)
			return
		case <-ticker.C:
			l.TryAcquire(ctx)
		}
	}
}
