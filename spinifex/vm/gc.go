package vm

import (
	"context"
	"log/slog"
	"time"
)

// GCInterval is the cadence of the reality→desired garbage-collection sweep.
// Var (not const) so integration tests can shrink it.
var GCInterval = 2 * time.Minute

// ReaperScope separates resource classes a node owns alone (NodeLocal) from
// cluster-wide classes a single elected leader must sweep (ClusterWide).
type ReaperScope int

const (
	ScopeNodeLocal ReaperScope = iota
	ScopeClusterWide
)

// Reaper sweeps one managed resource class: enumerate live ("actual") state and
// reap whatever is absent from desired (KV) state. Sweep MUST be idempotent —
// reaping an already-absent resource is a no-op — and reports how many
// resources it reaped so the GC can log a per-class summary.
type Reaper interface {
	Class() string
	Scope() ReaperScope
	Sweep(ctx context.Context) (reaped int, err error)
}

// GarbageCollector is the shared reconciler GC backstop (ADR-0003 §3): the
// inverse of Restore. It runs every registered Reaper on startup and on a
// periodic tick, gated on KV health so it never reaps against a desired-state
// it cannot trust. ClusterWide reapers run only on the elected leader;
// NodeLocal reapers run on every node for the resources it owns.
type GarbageCollector struct {
	reapers       []Reaper
	kvHealthy     func() bool
	acquireLeader func() (release func(), elected bool)
}

// NewGarbageCollector builds a GC over the given reapers. kvHealthy gates every
// sweep: a nil probe is treated as always-healthy (single-node / test).
func NewGarbageCollector(kvHealthy func() bool, reapers ...Reaper) *GarbageCollector {
	return &GarbageCollector{reapers: reapers, kvHealthy: kvHealthy}
}

// WithLeaderElection wires the cluster-wide leader gate. When unset, every
// ClusterWide reaper is skipped (no elector available); NodeLocal reapers are
// unaffected.
func (g *GarbageCollector) WithLeaderElection(acquire func() (release func(), elected bool)) *GarbageCollector {
	g.acquireLeader = acquire
	return g
}

// Start runs an immediate sweep, then one every GCInterval until ctx is done.
// Call once per GC to avoid duplicating the goroutine.
func (g *GarbageCollector) Start(ctx context.Context) {
	go func() {
		g.SweepOnce(ctx)
		ticker := time.NewTicker(GCInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				g.SweepOnce(ctx)
			}
		}
	}()
}

// SweepOnce runs one full pass: every reaper once, KV-health gated. Split out so
// tests drive it deterministically without waiting for GCInterval.
func (g *GarbageCollector) SweepOnce(ctx context.Context) {
	if g.kvHealthy != nil && !g.kvHealthy() {
		// Hold, do not reap: an unreadable/partial desired-state cannot be
		// trusted to declare any actual resource an orphan (ADR-0003 §3).
		slog.Warn("vm/gc: KV unhealthy, holding sweep (will not reap against untrusted desired-state)")
		return
	}
	for _, r := range g.reapers {
		if r.Scope() == ScopeClusterWide {
			if g.acquireLeader == nil {
				continue
			}
			release, elected := g.acquireLeader()
			if !elected {
				continue
			}
			g.runReaper(ctx, r)
			release()
			continue
		}
		g.runReaper(ctx, r)
	}
}

func (g *GarbageCollector) runReaper(ctx context.Context, r Reaper) {
	reaped, err := r.Sweep(ctx)
	if err != nil {
		slog.Warn("vm/gc: reaper failed", "class", r.Class(), "err", err)
		return
	}
	if reaped > 0 {
		slog.Info("vm/gc: reaped orphans", "class", r.Class(), "count", reaped)
	}
}
