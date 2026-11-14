package featurelevel

import (
	"context"
	"sync"
)

// Gate admits N-only work. It counts a caller in flight before it reads the
// record, so a rollback that sees zero in flight and an acknowledged revision
// cannot be overtaken by a request that passed the check and then paused.
type Gate struct {
	store *Store

	mu       sync.Mutex
	inFlight map[int]int
}

// NewGate returns a gate over store.
func NewGate(store *Store) *Gate {
	return &Gate{store: store, inFlight: map[int]int{}}
}

// Admit reports whether level n is active: the recorded level is at least n and
// no rollback below n is in progress. The caller holds done until its obligation
// is durably recorded or abandoned, and calls it exactly once on every path,
// including when err is non-nil. A read failure returns an error and false; each
// feature decides its safe answer. It is one uncached read.
func (g *Gate) Admit(ctx context.Context, n int) (done func(), ok bool, err error) {
	g.mu.Lock()
	g.inFlight[n]++
	g.mu.Unlock()
	var once sync.Once
	done = func() {
		once.Do(func() {
			g.mu.Lock()
			g.inFlight[n]--
			g.mu.Unlock()
		})
	}

	rec, _, err := g.store.Read(ctx)
	if err != nil {
		return done, false, err
	}
	return done, active(rec, n), nil
}

func active(rec Record, n int) bool {
	if rec.Level < n {
		return false
	}
	return rec.Rollback == nil || rec.Rollback.Target >= n
}

// InFlight returns the callers currently admitted per level.
func (g *Gate) InFlight() map[int]int {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := map[int]int{}
	for n, c := range g.inFlight {
		if c > 0 {
			out[n] = c
		}
	}
	return out
}

// InFlightAbove counts the callers admitted for levels above target.
func (g *Gate) InFlightAbove(target int) int {
	total := 0
	for n, c := range g.InFlight() {
		if n > target {
			total += c
		}
	}
	return total
}
