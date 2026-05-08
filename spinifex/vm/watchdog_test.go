package vm

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// pendingInstance returns a VM in the supplied state with LaunchTime set
// to launchedAgo before now. now is the synthetic clock used by
// scanAndMarkStuckPending in tests.
func pendingInstance(id string, state InstanceState, launched time.Time) *VM {
	return &VM{
		ID:     id,
		Status: state,
		Instance: &ec2.Instance{
			LaunchTime: &launched,
		},
	}
}

func TestScanAndMarkStuckPending_EmptyMap(t *testing.T) {
	m, _, rt, _ := crashTestManager(t)
	m.scanAndMarkStuckPending(time.Now())
	assert.Empty(t, rt.snapshot(), "empty map must not invoke any transitions")
}

func TestScanAndMarkStuckPending_FreshPending_NotMarked(t *testing.T) {
	m, _, rt, _ := crashTestManager(t)

	now := time.Now()
	v := pendingInstance("i-fresh", StatePending, now)
	m.Insert(v)

	m.scanAndMarkStuckPending(now)

	assert.Empty(t, rt.snapshot(),
		"fresh pending instance (elapsed=0) must not be marked failed")
	assert.Equal(t, StatePending, m.Status(v))
}

func TestScanAndMarkStuckPending_BoundaryNotStuck(t *testing.T) {
	m, _, rt, _ := crashTestManager(t)

	now := time.Now()
	// Exactly at the timeout boundary — strict ">" comparison means equal
	// is not yet stuck.
	v := pendingInstance("i-boundary", StatePending, now.Add(-PendingWatchdogTimeout))
	m.Insert(v)

	m.scanAndMarkStuckPending(now)

	assert.Empty(t, rt.snapshot(),
		"instance exactly at the timeout boundary must not be marked stuck")
}

func TestScanAndMarkStuckPending_StuckPending_MarkedFailed(t *testing.T) {
	defer goleak.VerifyNone(t)
	m, _, rt, _ := crashTestManager(t)

	now := time.Now()
	v := pendingInstance("i-stuck", StatePending, now.Add(-PendingWatchdogTimeout-time.Minute))
	m.Insert(v)

	terminated := rt.waitFor(v.ID, StateTerminated)
	m.scanAndMarkStuckPending(now)

	assertStuckMarkedFailed(t, m, rt, v, terminated)
}

func TestScanAndMarkStuckPending_StuckProvisioning_MarkedFailed(t *testing.T) {
	defer goleak.VerifyNone(t)
	m, _, rt, _ := crashTestManager(t)

	now := time.Now()
	v := pendingInstance("i-prov-stuck", StateProvisioning, now.Add(-PendingWatchdogTimeout-time.Second))
	m.Insert(v)

	terminated := rt.waitFor(v.ID, StateTerminated)
	m.scanAndMarkStuckPending(now)

	assertStuckMarkedFailed(t, m, rt, v, terminated)
}

func TestScanAndMarkStuckPending_NoLaunchTime_NotMarked(t *testing.T) {
	m, _, rt, _ := crashTestManager(t)

	v := &VM{
		ID:     "i-no-launchtime",
		Status: StatePending,
		// Instance is nil → predicate must short-circuit safely.
	}
	m.Insert(v)

	m.scanAndMarkStuckPending(time.Now())

	assert.Empty(t, rt.snapshot(),
		"instance without LaunchTime must not be marked stuck")
}

func TestScanAndMarkStuckPending_OnlyPendingStatesScanned(t *testing.T) {
	m, _, rt, _ := crashTestManager(t)

	now := time.Now()
	long := now.Add(-PendingWatchdogTimeout - time.Hour)

	for _, state := range []InstanceState{StateRunning, StateStopped, StateStopping, StateTerminated} {
		v := pendingInstance("i-"+string(state), state, long)
		m.Insert(v)
	}

	m.scanAndMarkStuckPending(now)

	assert.Empty(t, rt.snapshot(),
		"non-pending states must not be marked stuck regardless of LaunchTime")
}

func TestStartPendingWatchdog_CtxCancelStopsGoroutine(t *testing.T) {
	// goleak fails the test if the watchdog goroutine outlives ctx.
	// Without this, a regression that ignored ctx.Done would still pass:
	// the harness reaps the leaked goroutine on test process exit.
	defer goleak.VerifyNone(t)

	m, _, _, _ := crashTestManager(t)

	ctx, cancel := context.WithCancel(t.Context())
	m.StartPendingWatchdog(ctx)
	cancel()
}

func assertStuckMarkedFailed(t *testing.T, m *Manager, rt *recordedTransitions, v *VM, terminated <-chan struct{}) {
	t.Helper()

	// MarkFailed transitions Pending/Provisioning → ShuttingDown
	// synchronously, then runs terminateCleanup + finalizeTerminated in a
	// goroutine. Block on the chan recordedTransitions closes once the
	// Terminated transition has landed and Status is published.
	select {
	case <-terminated:
	case <-time.After(10 * time.Second):
		t.Fatalf("stuck instance %s did not reach Terminated within 10s", v.ID)
	}

	assert.Equal(t, StateTerminated, m.Status(v))
	targets := rt.targets(v.ID)
	require.NotEmpty(t, targets)
	assert.Equal(t, StateShuttingDown, targets[0],
		"first transition must be ShuttingDown (set by MarkFailed)")
	assert.Contains(t, targets, StateTerminated,
		"terminal transition must land in Terminated")
}
