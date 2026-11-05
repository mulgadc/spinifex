package kvsync

import (
	"context"
	"sync"
	"time"
)

// FakeClock is a manual clock: time moves only on Advance, and a waiter fires
// once Advance has carried the clock to its deadline.
type FakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []fakeWaiter
	changed chan struct{}
}

type fakeWaiter struct {
	at time.Time
	ch chan time.Time
}

// NewFakeClock returns a clock reading an arbitrary fixed instant.
func NewFakeClock() *FakeClock {
	return &FakeClock{now: time.Unix(1_000_000, 0), changed: make(chan struct{})}
}

func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *FakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, fakeWaiter{at: c.now.Add(d), ch: ch})
	c.notifyLocked()
	return ch
}

// Advance moves the clock on by d and fires every waiter now due.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if w.at.After(c.now) {
			kept = append(kept, w)
			continue
		}
		w.ch <- c.now
	}
	c.waiters = kept
	c.notifyLocked()
}

// WaitForWaiters blocks until at least n waiters are pending or ctx ends, and
// reports whether they were.
func (c *FakeClock) WaitForWaiters(ctx context.Context, n int) bool {
	for {
		c.mu.Lock()
		count, changed := len(c.waiters), c.changed
		c.mu.Unlock()
		if count >= n {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-changed:
		}
	}
}

// Waiters reports how many waiters are pending.
func (c *FakeClock) Waiters() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

func (c *FakeClock) notifyLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

// NewLeaseWithClock is NewLease measuring time on c.
func NewLeaseWithClock(store KVStore, key, holder string, ttl time.Duration, c *FakeClock) *Lease {
	registerKey(store, key)
	return newLease(store, key, holder, ttl, c)
}

// SetGroupClock makes every Lease g hands out from now on measure time on c.
func SetGroupClock(g *LeaseGroup, c *FakeClock) { g.clock = c }
