package gateway_ec2_instance_test

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	gateway_ec2_instance "github.com/mulgadc/spinifex/spinifex/gateway/ec2/instance"
	"github.com/mulgadc/spinifex/spinifex/instancecache"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureShadowLogs redirects the default slog logger into a buffer at Debug
// level, so both the Info- and Warn-level divergence lines this package emits
// are visible to a test. Not run under t.Parallel(): slog.Default() is a
// package global.
func captureShadowLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// fakeLiveness is a nodeLiveness stand-in: a fixed table of node -> state,
// Unknown for anything not listed, matching Liveness.State's own behaviour
// for a node it has never heard of.
type fakeLiveness struct {
	states map[string]instancecache.NodeState
}

func (f *fakeLiveness) State(_ context.Context, node string) instancecache.NodeState {
	if f == nil {
		return instancecache.NodeUnknown
	}
	if s, ok := f.states[node]; ok {
		return s
	}
	return instancecache.NodeUnknown
}

// panicCache is a CacheReader whose every method panics, standing in for a
// cache path that fails catastrophically rather than merely erroring.
type panicCache struct{}

func (panicCache) List(context.Context, string) ([]*vm.VM, bool) { panic("panicCache: List") }
func (panicCache) Get(context.Context, string) (*vm.VM, error)   { panic("panicCache: Get") }
func (panicCache) Degraded() bool                                { return false }

// erroringCache answers Get with an error for every ID, modelling a cache
// path that fails cleanly (a KV read failure) rather than panicking.
type erroringCache struct{}

func (erroringCache) List(context.Context, string) ([]*vm.VM, bool) { return nil, true }
func (erroringCache) Get(context.Context, string) (*vm.VM, error) {
	return nil, assert.AnError
}
func (erroringCache) Degraded() bool { return false }

// blockingCache signals acquired the instant its List is entered, then
// blocks until release is closed. It lets a test prove a slot was taken
// before racing a second caller against it, with no sleep-based timing.
type blockingCache struct {
	fakeCache

	acquired chan struct{}
	release  chan struct{}
}

func (b *blockingCache) List(ctx context.Context, accountID string) ([]*vm.VM, bool) {
	close(b.acquired)
	<-b.release
	return b.fakeCache.List(ctx, accountID)
}

func shadowInput(ids ...string) *ec2.DescribeInstancesInput {
	input := &ec2.DescribeInstancesInput{}
	for _, id := range ids {
		input.InstanceIds = append(input.InstanceIds, &id)
	}
	return input
}

const shadowAZ = "us-east-1a"

// A cache that has not completed its first sync answers "no answer"; the
// shadow path must treat that as nothing to compare, not as a divergence.
func TestShadowComparator_Run_CacheNotReady_NoDivergenceLogged(t *testing.T) {
	logs := captureShadowLogs(t)
	cache := &fakeCache{ready: false, byID: map[string]*vm.VM{}}
	comparator := gateway_ec2_instance.NewShadowComparator(time.Minute)

	comparator.Run(context.Background(), cache, nil, shadowInput(), cacheTestAccount, shadowAZ, gateway_ec2_instance.Snapshot{"i-1": "running"})

	assert.Empty(t, logs.String(), "a not-ready cache must not produce a divergence log")
}

// DescribeFromCache's own client-error path (a malformed filter) must be
// surfaced as a warning, not silently dropped or panicked on.
func TestShadowComparator_Run_CachePathErrors_LoggedNotPanicked(t *testing.T) {
	logs := captureShadowLogs(t)
	cache := &erroringCache{}
	comparator := gateway_ec2_instance.NewShadowComparator(time.Minute)

	require.NotPanics(t, func() {
		comparator.Run(context.Background(), cache, nil, shadowInput("i-1"), cacheTestAccount, shadowAZ, gateway_ec2_instance.Snapshot{"i-1": "running"})
	})

	assert.Contains(t, logs.String(), "cache path errored")
}

// A panic anywhere in the cache path must never escape Run: the customer
// already has the fan-out's answer by the time this runs.
func TestShadowComparator_Run_CachePathPanics_Recovered(t *testing.T) {
	logs := captureShadowLogs(t)
	comparator := gateway_ec2_instance.NewShadowComparator(time.Minute)

	require.NotPanics(t, func() {
		comparator.Run(context.Background(), panicCache{}, nil, shadowInput(), cacheTestAccount, shadowAZ, gateway_ec2_instance.Snapshot{"i-1": "running"})
	})

	assert.Contains(t, logs.String(), "recovered from panic")
}

// A cache-only instance whose owning node is not live is exactly the case
// this cache path exists for: expected, and evidence the fix works.
func TestShadowComparator_Run_ExpectedFix_NodeNotLive(t *testing.T) {
	logs := captureShadowLogs(t)
	v := cacheVM("i-cacheonly", cacheTestAccount, vm.StateRunning, vm.DesiredRunning)
	v.LastNode = "node-down"
	cache := &fakeCache{ready: true, byID: map[string]*vm.VM{"i-cacheonly": v}}
	liveness := &fakeLiveness{states: map[string]instancecache.NodeState{"node-down": instancecache.NodeStale}}
	comparator := gateway_ec2_instance.NewShadowComparator(time.Minute)

	comparator.Run(context.Background(), cache, liveness, shadowInput(), cacheTestAccount, shadowAZ, gateway_ec2_instance.Snapshot{})

	out := logs.String()
	assert.Contains(t, out, "class=expected_fix")
	assert.Contains(t, out, "level=INFO")
	assert.Contains(t, out, "i-cacheonly")
}

// A drain-stopped instance (status=Stopped, desired=Running) the fan-out
// still reports and the cache path deliberately excludes is a transient,
// expected difference — not a bug.
func TestShadowComparator_Run_ExpectedDrainWindow(t *testing.T) {
	logs := captureShadowLogs(t)
	drained := cacheVM("i-drained", cacheTestAccount, vm.StateStopped, vm.DesiredRunning)
	require.False(t, drained.OperatorStopped())
	cache := &fakeCache{ready: true, byID: map[string]*vm.VM{"i-drained": drained}}
	comparator := gateway_ec2_instance.NewShadowComparator(time.Minute)

	comparator.Run(context.Background(), cache, nil, shadowInput(), cacheTestAccount, shadowAZ, gateway_ec2_instance.Snapshot{"i-drained": "stopped"})

	out := logs.String()
	assert.Contains(t, out, "class=expected_drain_window")
	assert.Contains(t, out, "level=INFO")
}

// A ManagedBy instance the fan-out's (pre-fix) stopped path still shows a
// non-Global caller, and the cache path correctly hides, is the deliberate
// visibility tightening working as intended.
func TestShadowComparator_Run_ExpectedVisibilityTightening(t *testing.T) {
	logs := captureShadowLogs(t)
	owner := "111122223333"
	managed := cacheVM("i-managed", owner, vm.StateStopped, vm.DesiredStopped)
	managed.ManagedBy = "elbv2"
	require.True(t, managed.OperatorStopped())
	cache := &fakeCache{ready: true, byID: map[string]*vm.VM{"i-managed": managed}}
	comparator := gateway_ec2_instance.NewShadowComparator(time.Minute)

	comparator.Run(context.Background(), cache, nil, shadowInput(), owner, shadowAZ, gateway_ec2_instance.Snapshot{"i-managed": "stopped"})

	out := logs.String()
	assert.Contains(t, out, "class=expected_visibility_tightening")
	assert.Contains(t, out, "level=INFO")
}

// A field-value mismatch on an instance both sides serve, while the owning
// node is live and answering, is an ordinary KV-write race on first
// observation: the node updates before the KV write that the cache's watch
// picks up, so it starts as a propagation candidate and does not block. If
// it survives its (short) grace with the node still live, that race story
// no longer holds, and it becomes unexplained rather than persistent — it
// should have converged well inside one resync, so surviving means
// something is actually wrong.
func TestShadowComparator_Run_LiveNodeFieldMismatch_PropagatesThenUnexplained(t *testing.T) {
	comparator := gateway_ec2_instance.NewShadowComparator(30 * time.Millisecond)
	v := cacheVM("i-both", cacheTestAccount, vm.StateRunning, vm.DesiredRunning)
	v.LastNode = "node-1"
	cache := &fakeCache{ready: true, byID: map[string]*vm.VM{"i-both": v}}
	liveness := &fakeLiveness{states: map[string]instancecache.NodeState{"node-1": instancecache.NodeLive}}
	input := shadowInput()
	fanoutSnap := gateway_ec2_instance.Snapshot{"i-both": "stopping"}

	firstLogs := captureShadowLogs(t)
	comparator.Run(context.Background(), cache, liveness, input, cacheTestAccount, shadowAZ, fanoutSnap)
	out := firstLogs.String()
	assert.Contains(t, out, "class=expected_propagation",
		"a live-node mismatch on first observation is an ordinary KV-write race, not immediately unexplained")
	assert.Contains(t, out, "level=INFO", "a first-observation propagation candidate must not block the flip")
	assert.NotContains(t, out, "class=unexplained")

	time.Sleep(20 * time.Millisecond) // outlive the short live-node grace

	secondLogs := captureShadowLogs(t)
	comparator.Run(context.Background(), cache, liveness, input, cacheTestAccount, shadowAZ, fanoutSnap)
	out = secondLogs.String()
	assert.Contains(t, out, "class=unexplained",
		"a live-node mismatch surviving its grace should have converged inside one resync; unexplained, not persistent")
	assert.Contains(t, out, "level=WARN")
	assert.NotContains(t, out, "class=persistent")
}

// The same field mismatch, but the owning node is not confirmed live, has a
// plausible propagation story and must not be immediately branded
// unexplained.
func TestShadowComparator_Run_FieldMismatch_NodeNotLive_IsPropagationNotUnexplained(t *testing.T) {
	logs := captureShadowLogs(t)
	v := cacheVM("i-both", cacheTestAccount, vm.StateRunning, vm.DesiredRunning)
	v.LastNode = "node-1"
	cache := &fakeCache{ready: true, byID: map[string]*vm.VM{"i-both": v}}
	liveness := &fakeLiveness{states: map[string]instancecache.NodeState{"node-1": instancecache.NodeStale}}
	comparator := gateway_ec2_instance.NewShadowComparator(time.Minute)

	comparator.Run(context.Background(), cache, liveness, shadowInput(), cacheTestAccount, shadowAZ, gateway_ec2_instance.Snapshot{"i-both": "stopping"})

	out := logs.String()
	assert.Contains(t, out, "class=expected_propagation")
	assert.NotContains(t, out, "class=unexplained")
}

// A divergence still present after it has outlived one resync interval
// stops being an expected propagation lag and becomes persistent, which
// blocks the flip even though nothing about it looks structurally wrong.
func TestShadowComparator_Run_UnresolvedPastResyncInterval_BecomesPersistent(t *testing.T) {
	comparator := gateway_ec2_instance.NewShadowComparator(5 * time.Millisecond)
	cache := &fakeCache{ready: true, byID: map[string]*vm.VM{}}
	input := shadowInput()
	fanoutSnap := gateway_ec2_instance.Snapshot{"i-new": "pending"}

	firstLogs := captureShadowLogs(t)
	comparator.Run(context.Background(), cache, nil, input, cacheTestAccount, shadowAZ, fanoutSnap)
	assert.Contains(t, firstLogs.String(), "class=expected_propagation",
		"first sighting of an unexplained presence gap must start as a propagation candidate")

	time.Sleep(20 * time.Millisecond)

	secondLogs := captureShadowLogs(t)
	comparator.Run(context.Background(), cache, nil, input, cacheTestAccount, shadowAZ, fanoutSnap)
	assert.Contains(t, secondLogs.String(), "class=persistent",
		"a divergence still present after outliving the resync interval must escalate to persistent")
}

// getMissCache mirrors fakeCache for List (so an entry still shows up in a
// cache-only diff) but Get always misses, modelling a lookup racing an
// eviction between the two calls — the one way evalCacheOnly's expected_fix
// short-circuit can be bypassed without a live node.
type getMissCache struct {
	fakeCache
}

func (*getMissCache) Get(context.Context, string) (*vm.VM, error) { return nil, nil }

// A cache-only divergence with no resolvable node state (Get misses, so
// liveness never enters the picture) still escalates to persistent after
// outliving the resync interval, the same as a fanout-only one.
func TestShadowComparator_Run_CacheOnly_UnresolvedPastResyncInterval_BecomesPersistent(t *testing.T) {
	comparator := gateway_ec2_instance.NewShadowComparator(5 * time.Millisecond)
	v := cacheVM("i-cacheonly", cacheTestAccount, vm.StateRunning, vm.DesiredRunning)
	cache := &getMissCache{fakeCache{ready: true, byID: map[string]*vm.VM{"i-cacheonly": v}}}
	input := shadowInput()

	firstLogs := captureShadowLogs(t)
	comparator.Run(context.Background(), cache, nil, input, cacheTestAccount, shadowAZ, gateway_ec2_instance.Snapshot{})
	assert.Contains(t, firstLogs.String(), "class=expected_propagation",
		"first sighting of a cache-only divergence must start as a propagation candidate")

	time.Sleep(20 * time.Millisecond)

	secondLogs := captureShadowLogs(t)
	comparator.Run(context.Background(), cache, nil, input, cacheTestAccount, shadowAZ, gateway_ec2_instance.Snapshot{})
	assert.Contains(t, secondLogs.String(), "class=persistent",
		"a cache-only divergence still present after outliving the resync interval must escalate to persistent")
}

// The case env19 nearly hit: a cache-only presence divergence on a node
// liveness still reports live, observed past the (short) resync interval
// but still inside the (much longer) staleness window, must not become
// persistent — liveness has not yet had the chance to call the node stale
// and hand this to expected_fix.
func TestShadowComparator_Run_CacheOnly_LiveNodePastResyncInsideStaleness_NotPersistent(t *testing.T) {
	comparator := gateway_ec2_instance.NewShadowComparatorForTest(5*time.Millisecond, 50*time.Millisecond, 32)
	v := cacheVM("i-cacheonly", cacheTestAccount, vm.StateRunning, vm.DesiredRunning)
	v.LastNode = "node-1"
	cache := &fakeCache{ready: true, byID: map[string]*vm.VM{"i-cacheonly": v}}
	liveness := &fakeLiveness{states: map[string]instancecache.NodeState{"node-1": instancecache.NodeLive}}
	input := shadowInput()

	firstLogs := captureShadowLogs(t)
	comparator.Run(context.Background(), cache, liveness, input, cacheTestAccount, shadowAZ, gateway_ec2_instance.Snapshot{})
	assert.Contains(t, firstLogs.String(), "class=expected_propagation")

	time.Sleep(20 * time.Millisecond) // past the 5ms resync interval, still inside the 50ms staleness window

	secondLogs := captureShadowLogs(t)
	comparator.Run(context.Background(), cache, liveness, input, cacheTestAccount, shadowAZ, gateway_ec2_instance.Snapshot{})
	out := secondLogs.String()
	assert.Contains(t, out, "class=expected_propagation",
		"a live node's cache-only divergence past the resync interval must not become persistent before its staleness window elapses")
	assert.NotContains(t, out, "class=persistent")
}

// Guards the constant that bounds live-node persistence promotion so it can
// never fall below instancecache.NodeStaleAfter again — the same drift
// concern daemon/heartbeat_test.go guards for the heartbeat interval copy.
func TestLiveNodePersistenceDeadlineNeverBeatsLiveness(t *testing.T) {
	deadline := gateway_ec2_instance.LiveNodePersistenceDeadlineForTest()
	if deadline < instancecache.NodeStaleAfter {
		t.Fatalf("live-node persistence deadline %v is shorter than instancecache.NodeStaleAfter %v; "+
			"a divergence could be judged persistent before liveness ever gets to call the node stale",
			deadline, instancecache.NodeStaleAfter)
	}
}

// A field-value mismatch still present after outliving the resync interval
// escalates to persistent the same as a presence divergence. Uses a stale
// owning node so the mismatch starts as expected_propagation rather than
// being immediately branded unexplained.
func TestShadowComparator_Run_Mismatch_UnresolvedPastResyncInterval_BecomesPersistent(t *testing.T) {
	comparator := gateway_ec2_instance.NewShadowComparator(5 * time.Millisecond)
	v := cacheVM("i-both", cacheTestAccount, vm.StateRunning, vm.DesiredRunning)
	v.LastNode = "node-1"
	cache := &fakeCache{ready: true, byID: map[string]*vm.VM{"i-both": v}}
	liveness := &fakeLiveness{states: map[string]instancecache.NodeState{"node-1": instancecache.NodeStale}}
	input := shadowInput()
	fanoutSnap := gateway_ec2_instance.Snapshot{"i-both": "stopping"}

	firstLogs := captureShadowLogs(t)
	comparator.Run(context.Background(), cache, liveness, input, cacheTestAccount, shadowAZ, fanoutSnap)
	assert.Contains(t, firstLogs.String(), "class=expected_propagation",
		"first sighting of a field mismatch on a not-live node must start as a propagation candidate")

	time.Sleep(20 * time.Millisecond)

	secondLogs := captureShadowLogs(t)
	comparator.Run(context.Background(), cache, liveness, input, cacheTestAccount, shadowAZ, fanoutSnap)
	assert.Contains(t, secondLogs.String(), "class=persistent",
		"a field mismatch still present after outliving the resync interval must escalate to persistent")
}

// A propagation candidate that converges (both sides agree on the next
// whole-account run) must be forgotten, not carried forward — otherwise a
// stale entry could later resurface at a false "age" and be mislabelled
// persistent on a divergence that is actually brand new.
func TestShadowComparator_Run_ConvergedPropagation_ResetsOnRecurrence(t *testing.T) {
	comparator := gateway_ec2_instance.NewShadowComparator(30 * time.Millisecond)
	cache := &fakeCache{ready: true, byID: map[string]*vm.VM{}}
	input := shadowInput() // filters-only: whole-account scope, prunable

	logs1 := captureShadowLogs(t)
	comparator.Run(context.Background(), cache, nil, input, cacheTestAccount, shadowAZ, gateway_ec2_instance.Snapshot{"i-flap": "pending"})
	assert.Contains(t, logs1.String(), "class=expected_propagation")

	time.Sleep(50 * time.Millisecond) // outlive the resync interval while converged

	logs2 := captureShadowLogs(t)
	comparator.Run(context.Background(), cache, nil, input, cacheTestAccount, shadowAZ, gateway_ec2_instance.Snapshot{}) // converged: no divergence this run
	assert.NotContains(t, logs2.String(), "class=", "a converged run must not log a divergence")

	logs3 := captureShadowLogs(t)
	comparator.Run(context.Background(), cache, nil, input, cacheTestAccount, shadowAZ, gateway_ec2_instance.Snapshot{"i-flap": "pending"})
	assert.Contains(t, logs3.String(), "class=expected_propagation",
		"a divergence recurring after resolution must start over as a fresh propagation candidate")
	assert.NotContains(t, logs3.String(), "class=persistent")
}

// pruneTracked must only forgive keys within a run's own scope: an
// explicit-ID request must not prune a divergence tracked for a different
// instance in the same account, even though the account-wide tracker holds
// both.
func TestShadowComparator_Run_ExplicitIDRequest_DoesNotPruneOutOfScopeDivergence(t *testing.T) {
	comparator := gateway_ec2_instance.NewShadowComparator(30 * time.Millisecond)
	cache := &fakeCache{ready: true, byID: map[string]*vm.VM{}}

	seedLogs := captureShadowLogs(t)
	comparator.Run(context.Background(), cache, nil, shadowInput(), cacheTestAccount, shadowAZ, gateway_ec2_instance.Snapshot{"i-other": "pending"})
	assert.Contains(t, seedLogs.String(), "class=expected_propagation")

	// An explicit-ID request for an unrelated, converged instance must not
	// touch i-other's tracked state.
	comparator.Run(context.Background(), cache, nil, shadowInput("i-target"), cacheTestAccount, shadowAZ, gateway_ec2_instance.Snapshot{})

	time.Sleep(50 * time.Millisecond) // outlive the resync interval

	finalLogs := captureShadowLogs(t)
	comparator.Run(context.Background(), cache, nil, shadowInput(), cacheTestAccount, shadowAZ, gateway_ec2_instance.Snapshot{"i-other": "pending"})
	assert.Contains(t, finalLogs.String(), "class=persistent",
		"an explicit-ID request for a different instance must not have reset i-other's divergence age")
}

// The concurrency budget must degrade to "skipped" under load rather than
// let comparisons pile up as a growing goroutine backlog.
func TestShadowComparator_Run_ConcurrencyBudgetExhausted_Skips(t *testing.T) {
	release := make(chan struct{})
	cache := &blockingCache{
		fakeCache: fakeCache{ready: true, byID: map[string]*vm.VM{}},
		acquired:  make(chan struct{}),
		release:   release,
	}
	comparator := gateway_ec2_instance.NewShadowComparatorForTest(time.Minute, 0, 1)

	done := make(chan struct{})
	go func() {
		comparator.Run(context.Background(), cache, nil, shadowInput(), cacheTestAccount, shadowAZ, gateway_ec2_instance.Snapshot{})
		close(done)
	}()
	<-cache.acquired // the first Run has taken the only slot and is blocked inside List

	logs := captureShadowLogs(t)
	comparator.Run(context.Background(), cache, nil, shadowInput(), cacheTestAccount, shadowAZ, gateway_ec2_instance.Snapshot{})
	assert.Contains(t, logs.String(), "concurrency budget exhausted")

	close(release)
	<-done
}

// SnapshotDescribeOutput must extract exactly what a served answer showed,
// since the shadow path compares against this rather than the live output.
func TestSnapshotDescribeOutput(t *testing.T) {
	out := &ec2.DescribeInstancesOutput{
		Reservations: []*ec2.Reservation{
			{Instances: []*ec2.Instance{
				{InstanceId: strPtr("i-1"), State: &ec2.InstanceState{Name: strPtr("running")}},
				{InstanceId: strPtr("i-2"), State: nil},
			}},
		},
	}

	snap := gateway_ec2_instance.SnapshotDescribeOutput(out)
	assert.Equal(t, gateway_ec2_instance.Snapshot{"i-1": "running", "i-2": ""}, snap)
	assert.Empty(t, gateway_ec2_instance.SnapshotDescribeOutput(nil))
}

func strPtr(s string) *string { return &s }

// Verifies the GlobalAccountID import is exercised the same way the
// describe_from_cache tests use it, so a caller reading this file alongside
// that one sees a consistent visibility story.
func TestShadowComparator_GlobalCaller_SeesManagedByInstance_NoTighteningClaimed(t *testing.T) {
	logs := captureShadowLogs(t)
	systemOwned := cacheVM("i-lb", utils.GlobalAccountID, vm.StateStopped, vm.DesiredStopped)
	systemOwned.ManagedBy = "elbv2"
	cache := &fakeCache{ready: true, byID: map[string]*vm.VM{"i-lb": systemOwned}}
	comparator := gateway_ec2_instance.NewShadowComparator(time.Minute)

	comparator.Run(context.Background(), cache, nil, shadowInput(), utils.GlobalAccountID, shadowAZ, gateway_ec2_instance.Snapshot{"i-lb": "stopped"})

	assert.NotContains(t, logs.String(), "class=", "Global sees its own managed instance on both sides, so there is nothing to diverge on")
}
