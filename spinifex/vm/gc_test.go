package vm

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// countingReaper is a test Reaper that records how many times Sweep ran and
// returns canned (reaped, err) values.
type countingReaper struct {
	class  string
	scope  ReaperScope
	reaped int
	err    error
	calls  atomic.Int64
}

func (c *countingReaper) Class() string      { return c.class }
func (c *countingReaper) Scope() ReaperScope { return c.scope }
func (c *countingReaper) Sweep(context.Context) (int, error) {
	c.calls.Add(1)
	return c.reaped, c.err
}

func TestSweepOnce_HoldsWhenKVUnhealthy(t *testing.T) {
	r := &countingReaper{class: "x", scope: ScopeNodeLocal}
	gc := NewGarbageCollector(func() bool { return false }, r)

	gc.SweepOnce(context.Background())

	assert.Zero(t, r.calls.Load(), "no reaper may run while KV is unhealthy")
}

func TestSweepOnce_RunsNodeLocalWhenHealthy(t *testing.T) {
	r := &countingReaper{class: "x", scope: ScopeNodeLocal, reaped: 2}
	gc := NewGarbageCollector(func() bool { return true }, r)

	gc.SweepOnce(context.Background())

	assert.Equal(t, int64(1), r.calls.Load())
}

func TestSweepOnce_NilHealthProbeTreatedHealthy(t *testing.T) {
	r := &countingReaper{class: "x", scope: ScopeNodeLocal}
	gc := NewGarbageCollector(nil, r)

	gc.SweepOnce(context.Background())

	assert.Equal(t, int64(1), r.calls.Load())
}

func TestSweepOnce_ReaperErrorDoesNotAbortOthers(t *testing.T) {
	bad := &countingReaper{class: "bad", scope: ScopeNodeLocal, err: errors.New("boom")}
	good := &countingReaper{class: "good", scope: ScopeNodeLocal}
	gc := NewGarbageCollector(nil, bad, good)

	gc.SweepOnce(context.Background())

	assert.Equal(t, int64(1), bad.calls.Load())
	assert.Equal(t, int64(1), good.calls.Load(), "a failing reaper must not skip later reapers")
}

func TestSweepOnce_ClusterScopeSkippedWithoutElector(t *testing.T) {
	r := &countingReaper{class: "cluster", scope: ScopeClusterWide}
	gc := NewGarbageCollector(nil, r)

	gc.SweepOnce(context.Background())

	assert.Zero(t, r.calls.Load(), "cluster-wide reaper must not run without a leader elector")
}

func TestSweepOnce_ClusterScopeRunsOnlyWhenElected(t *testing.T) {
	r := &countingReaper{class: "cluster", scope: ScopeClusterWide}
	var released atomic.Int64
	elected := true
	gc := NewGarbageCollector(nil, r).WithLeaderElection(func() (func(), bool) {
		return func() { released.Add(1) }, elected
	})

	gc.SweepOnce(context.Background())
	assert.Equal(t, int64(1), r.calls.Load(), "elected leader must run the cluster reaper")
	assert.Equal(t, int64(1), released.Load(), "leader lock must be released after the sweep")

	elected = false
	gc.SweepOnce(context.Background())
	assert.Equal(t, int64(1), r.calls.Load(), "a non-leader must not run the cluster reaper")
}

func TestStart_SweepsImmediatelyThenStopsOnCtxCancel(t *testing.T) {
	old := GCInterval
	GCInterval = 5 * time.Millisecond
	defer func() { GCInterval = old }()

	var mu sync.Mutex
	r := &countingReaper{class: "x", scope: ScopeNodeLocal}
	gc := NewGarbageCollector(nil, r)

	ctx, cancel := context.WithCancel(context.Background())
	gc.Start(ctx)

	assert.Eventually(t, func() bool { return r.calls.Load() >= 1 }, time.Second, 2*time.Millisecond,
		"Start must sweep immediately, not wait a full interval")

	cancel()
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	stopped := r.calls.Load()
	mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, stopped, r.calls.Load(), "ctx cancel must stop the sweep loop")
}
