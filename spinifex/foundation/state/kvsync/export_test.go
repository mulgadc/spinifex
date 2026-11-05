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
	// deadline marks a Deadline timer, which the waiter counts leave out.
	deadline bool
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
	return c.add(d, false)
}

func (c *FakeClock) Deadline(d time.Duration) (<-chan time.Time, func()) {
	ch := c.add(d, true)
	return ch, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		kept := c.waiters[:0]
		for _, w := range c.waiters {
			if w.ch != ch {
				kept = append(kept, w)
			}
		}
		c.waiters = kept
		c.notifyLocked()
	}
}

func (c *FakeClock) add(d time.Duration, deadline bool) chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, fakeWaiter{at: c.now.Add(d), ch: ch, deadline: deadline})
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

// WaitForWaiters blocks until at least n After waiters are pending or ctx
// ends, and reports whether they were.
func (c *FakeClock) WaitForWaiters(ctx context.Context, n int) bool {
	return c.waitFor(ctx, n, false)
}

// WaitForDeadlines is WaitForWaiters for Deadline timers.
func (c *FakeClock) WaitForDeadlines(ctx context.Context, n int) bool {
	return c.waitFor(ctx, n, true)
}

func (c *FakeClock) waitFor(ctx context.Context, n int, deadline bool) bool {
	for {
		c.mu.Lock()
		count, changed := c.countLocked(deadline), c.changed
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

// Waiters reports how many After waiters are pending.
func (c *FakeClock) Waiters() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.countLocked(false)
}

func (c *FakeClock) countLocked(deadline bool) int {
	n := 0
	for _, w := range c.waiters {
		if w.deadline == deadline {
			n++
		}
	}
	return n
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
