package featurelevel

import (
	"context"
	"maps"
	"sync"
)

// Gate admits N-only work. It counts a caller in flight before it reads the
// record, so a rollback that sees zero in flight and an acknowledged revision
// cannot be overtaken by a request that passed the check and then paused.
type Gate struct {
	store *Store

	mu       sync.Mutex
	inFlight map[int]int
	holding  map[int]func() int
}

// NewGate returns a gate over store.
func NewGate(store *Store) *Gate {
	return &Gate{store: store, inFlight: map[int]int{}, holding: map[int]func() int{}}
}

// SetHolding installs the count of level n's obligations this node holds
// outside any request, such as installed datapath state, which its status
// reports for a drain check to read.
func (g *Gate) SetHolding(n int, count func() int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.holding[n] = count
}

// Holding returns the node-local obligations per level, omitting levels that
// hold none.
func (g *Gate) Holding() map[int]int {
	g.mu.Lock()
	counts := make(map[int]func() int, len(g.holding))
	maps.Copy(counts, g.holding)
	g.mu.Unlock()
	out := map[int]int{}
	for n, f := range counts {
		if c := f(); c > 0 {
			out[n] = c
		}
	}
	return out
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
	return done, rec.Active(n), nil
}

// Active reports whether level n is active without admitting anything, for a
// read that creates no obligation. It is one uncached read.
func (g *Gate) Active(ctx context.Context, n int) (bool, error) {
	rec, _, err := g.store.Read(ctx)
	if err != nil {
		return false, err
	}
	return rec.Active(n), nil
}

// Recorded reports whether level n is recorded, even while a rollback below n
// waits on its drain. A read of resources the level created uses it, so those
// resources stay visible until the drain lets the rollback finish.
func (g *Gate) Recorded(ctx context.Context, n int) (bool, error) {
	rec, _, err := g.store.Read(ctx)
	if err != nil {
		return false, err
	}
	return rec.Level >= n, nil
}

// Active reports whether level n is active in the record: recorded at least n,
// with no rollback below n in progress.
func (rec Record) Active(n int) bool {
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
