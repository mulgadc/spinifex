package kvsync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// releaseTimeout bounds Unlock's delete, which runs on a detached context so
// that a cancelled caller still frees the key for the next holder.
const releaseTimeout = 5 * time.Second

// Lease is ownership of a key that lapses unless its holder renews it. Within
// a process it excludes like a sync.Mutex: while held, TryLock from any call
// site returns *HeldError. A Lease is reusable after Unlock or after Lost closes.
//
// Expiry is never read from the store. A key another holder has is taken over
// only once this Lease has seen the same revision for a full TTL on its own
// monotonic clock, so skew between nodes cannot shorten a lease.
type Lease struct {
	store  KVStore
	key    string
	holder string
	ttl    time.Duration
	clock  clock

	// mu guards every field below.
	mu sync.Mutex
	// acquiring is set while a TryLock is between its read and its write.
	acquiring bool
	cur       *acquisition
	// lost is the current or most recent acquisition's channel.
	lost chan struct{}
	// obsRev is another holder's revision as first seen at obsAt.
	obsRev uint64
	obsAt  time.Time
}

// acquisition is one holding of a Lease, from TryLock to Unlock or loss.
type acquisition struct {
	gen  uint64
	lost chan struct{}
	stop chan struct{}
	done chan struct{}
	// rev and landed, guarded by Lease.mu, are the last write that landed and
	// when it was sent.
	rev    uint64
	landed time.Time
}

// NewLease returns the lease on key for holder. It panics if key is invalid or
// already constructed against store, if holder is empty, or if ttl is not positive.
func NewLease(store KVStore, key, holder string, ttl time.Duration) *Lease {
	registerKey(store, key)
	return newLease(store, key, holder, ttl, realClock{})
}

func newLease(store KVStore, key, holder string, ttl time.Duration, c clock) *Lease {
	if holder == "" {
		panic("kvsync: empty holder") //nolint:forbidigo // Constructing a lock wrongly is a wiring error that must fail at startup, not at the first election.
	}
	if ttl <= 0 {
		panic("kvsync: lease ttl must be positive") //nolint:forbidigo // Constructing a lock wrongly is a wiring error that must fail at startup, not at the first election.
	}
	closed := make(chan struct{})
	close(closed)
	return &Lease{store: store, key: key, holder: holder, ttl: ttl, clock: c, lost: closed}
}

// Key returns the store key the lease is held on.
func (l *Lease) Key() string { return l.key }

// TryLock makes one attempt to acquire the key, returning the token of its own
// write. Absent, the key is created; naming this holder, it is adopted by CAS;
// naming another, it is taken over once its revision has been seen for a TTL.
func (l *Lease) TryLock(ctx context.Context) (Token, error) {
	l.mu.Lock()
	if l.cur != nil || l.acquiring {
		cur := Token{Holder: l.holder}
		if l.cur != nil {
			cur.Generation = l.cur.gen
		}
		l.mu.Unlock()
		return Token{}, &HeldError{Current: cur}
	}
	l.acquiring = true
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		l.acquiring = false
		l.mu.Unlock()
	}()

	rev, sent, err := l.acquire(ctx)
	if err != nil {
		return Token{}, err
	}
	return l.install(ctx, rev, sent), nil
}

// acquire returns the revision of its write and when the write was sent. A
// peer's TTL starts no earlier than the send, so validity is counted from it,
// and a grant whose response took 2*ttl/3 is refused rather than installed.
func (l *Lease) acquire(ctx context.Context) (uint64, time.Time, error) {
	cur, rev, ok, err := read(ctx, l.store, l.key)
	if err != nil {
		return 0, time.Time{}, err
	}
	if ok && cur.Holder != l.holder && !l.observedFor(rev) {
		return 0, time.Time{}, &HeldError{Current: cur}
	}
	validity := 2 * l.ttl / 3
	sent := l.clock.Now()
	casCtx, cancel := context.WithTimeout(ctx, validity)
	next, err := l.store.CompareAndSwap(casCtx, l.key, encode(l.holder, 0), rev)
	cancel()
	if errors.Is(err, ErrRevisionMismatch) {
		return 0, time.Time{}, heldAfterConflict(ctx, l.store, l.key)
	}
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("kvsync: lock %s: %w", l.key, err)
	}
	if l.clock.Now().Sub(sent) >= validity {
		return 0, time.Time{}, fmt.Errorf("kvsync: lock %s: grant arrived after its validity: %w", l.key, ErrUnavailable)
	}
	return next, sent, nil
}

// observedFor records rev as seen now if it is new, and reports whether it has
// been seen for a full TTL.
func (l *Lease) observedFor(rev uint64) bool {
	now := l.clock.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.obsRev != rev {
		l.obsRev, l.obsAt = rev, now
		return false
	}
	return now.Sub(l.obsAt) >= l.ttl
}

// install records an acquisition at rev, valid from sent, and starts its
// renewal, which ignores the caller's cancellation and stops at Unlock.
func (l *Lease) install(ctx context.Context, rev uint64, sent time.Time) Token {
	a := &acquisition{
		gen:    rev,
		rev:    rev,
		landed: sent,
		lost:   make(chan struct{}),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	l.mu.Lock()
	l.cur, l.lost, l.obsRev = a, a.lost, 0
	l.mu.Unlock()
	go l.renew(context.WithoutCancel(ctx), a)
	return Token{Holder: l.holder, Generation: rev}
}

// renew rewrites the key by CAS ttl/3 after each write that landed was sent,
// until stopped. A mismatch loses the lease at once. An unreachable store is
// retried, and the lease is lost at 2*ttl/3 after that send, however long a CAS has been stuck.
func (l *Lease) renew(ctx context.Context, a *acquisition) {
	defer close(a.done)
	interval := l.ttl / 3
	validity := 2 * l.ttl / 3
	retry := max(l.ttl/12, time.Millisecond)
	var lastErr error
	for {
		l.mu.Lock()
		rev, landed := a.rev, a.landed
		l.mu.Unlock()
		deadline := landed.Add(validity)
		wait := max(0, landed.Add(interval).Sub(l.clock.Now()))
		if lastErr != nil {
			wait = min(retry, deadline.Sub(l.clock.Now()))
		}
		select {
		case <-a.stop:
			return
		case <-l.clock.After(wait):
		}
		budget := deadline.Sub(l.clock.Now())
		if budget <= 0 {
			l.lose(ctx, a, "renewal unavailable for two thirds of the ttl", lastErr)
			return
		}
		sent := l.clock.Now()
		next, err, inTime := l.swapWithin(ctx, a.gen, rev, budget, min(budget, interval))
		if !inTime {
			l.lose(ctx, a, "renewal did not answer within two thirds of the ttl", nil)
			return
		}
		switch {
		case err == nil:
			l.mu.Lock()
			// Stamped at the send: a peer may have seen the revision that early.
			a.rev, a.landed = next, sent
			l.mu.Unlock()
			lastErr = nil
		case errors.Is(err, ErrRevisionMismatch):
			l.lose(ctx, a, "renewal lost the key", err)
			return
		default:
			lastErr = err
		}
	}
}

// swapWithin runs one renewal CAS, giving up on it once budget has passed on
// the lease's clock, since a CAS can block past its own context. A late
// result is dropped: by then the lease has been given up.
func (l *Lease) swapWithin(ctx context.Context, gen, rev uint64, budget, timeout time.Duration) (uint64, error, bool) {
	type result struct {
		next uint64
		err  error
	}
	done := make(chan result, 1)
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	go func() {
		next, err := l.store.CompareAndSwap(attemptCtx, l.key, encode(l.holder, gen), rev)
		done <- result{next, err}
	}()
	expired, stop := l.clock.Deadline(budget)
	defer stop()
	select {
	case r := <-done:
		return r.next, r.err, true
	case <-expired:
		return 0, nil, false
	}
}

// lose ends a if it is still the current acquisition.
func (l *Lease) lose(ctx context.Context, a *acquisition, why string, err error) {
	l.mu.Lock()
	current := l.cur == a
	if current {
		l.cur = nil
		close(a.lost)
	}
	l.mu.Unlock()
	if current {
		slog.WarnContext(ctx, "kvsync: lease lost", "key", l.key, "holder", l.holder,
			"generation", a.gen, "reason", why, "err", err)
	}
}

// Unlock releases the acquisition held is the token of, and does nothing
// unless this Lease is still held at that generation, so a stale release never
// frees a later acquisition. It deletes at the last revision that landed.
func (l *Lease) Unlock(ctx context.Context, held Token) {
	l.mu.Lock()
	a := l.cur
	if a == nil || held.Holder != l.holder || held.Generation != a.gen {
		l.mu.Unlock()
		return
	}
	l.cur = nil
	close(a.lost)
	close(a.stop)
	l.mu.Unlock()

	// Waited for so that no renewal lands after the revision is read.
	<-a.done
	l.mu.Lock()
	rev := a.rev
	l.mu.Unlock()

	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	if err := l.store.CompareAndDelete(releaseCtx, l.key, rev); err != nil {
		slog.WarnContext(releaseCtx, "kvsync: lease release did not delete the key",
			"key", l.key, "holder", l.holder, "generation", a.gen, "err", err)
	}
}

// Lost returns a channel closed when the current acquisition ends, by loss or
// Unlock. Each acquisition has its own; before the first it is already closed.
func (l *Lease) Lost() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lost
}

// Held reports whether this Lease is currently held in this process.
func (l *Lease) Held() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cur != nil
}

// Holder reports the token stored at the key, and false when nobody holds it.
// A key whose holder has stopped renewing still reports that holder.
func (l *Lease) Holder(ctx context.Context) (Token, bool, error) {
	cur, _, ok, err := read(ctx, l.store, l.key)
	return cur, ok, err
}

// release unlocks the current acquisition, if any.
func (l *Lease) release(ctx context.Context) {
	l.mu.Lock()
	a := l.cur
	l.mu.Unlock()
	if a != nil {
		l.Unlock(ctx, Token{Holder: l.holder, Generation: a.gen})
	}
}

// LeaseGroup hands out per-resource leases under one registered prefix, one
// Lease per name for as long as the owner keeps it.
type LeaseGroup struct {
	store  KVStore
	prefix string
	holder string
	ttl    time.Duration
	clock  clock

	// mu guards leases.
	mu     sync.Mutex
	leases map[string]*Lease
}

// NewLeaseGroup registers prefix, which ends in ".", against store. It panics
// on an invalid or overlapping prefix, an empty holder or a non-positive ttl.
func NewLeaseGroup(store KVStore, prefix, holder string, ttl time.Duration) *LeaseGroup {
	if holder == "" {
		panic("kvsync: empty holder") //nolint:forbidigo // Constructing a lock wrongly is a wiring error that must fail at startup, not at the first election.
	}
	if ttl <= 0 {
		panic("kvsync: lease ttl must be positive") //nolint:forbidigo // Constructing a lock wrongly is a wiring error that must fail at startup, not at the first election.
	}
	registerPrefix(store, prefix)
	return &LeaseGroup{store: store, prefix: prefix, holder: holder, ttl: ttl, clock: realClock{}, leases: map[string]*Lease{}}
}

// Get returns the Lease for prefix+name, the same one until Forget.
func (g *LeaseGroup) Get(name string) *Lease {
	if err := validName("name", name); err != nil {
		panic(err) //nolint:forbidigo // Constructing a lock wrongly is a wiring error that must fail at startup, not at the first election.
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if l, ok := g.leases[name]; ok {
		return l
	}
	l := newLease(g.store, g.prefix+name, g.holder, g.ttl, g.clock)
	g.leases[name] = l
	return l
}

// Forget unlocks the name's current acquisition, if any, and drops the name,
// so a resource recreated under it starts from a fresh Lease.
func (g *LeaseGroup) Forget(name string) {
	g.mu.Lock()
	l := g.leases[name]
	delete(g.leases, name)
	g.mu.Unlock()
	if l != nil {
		l.release(context.Background())
	}
}
