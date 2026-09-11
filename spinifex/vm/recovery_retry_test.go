package vm

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A guest drain-stopped by a cluster update used to be lost for good if its
// relaunch lost a race with a still-starting dependency: MarkRecoveryFailed
// parked it in StateError and nothing ever reconsidered it. These tests cover
// the retry that now does, and the limits on it — an automatic relaunch that
// cannot tell "the store was down" from "this guest will not start" would turn
// one broken instance into a permanent relaunch loop.

// erroredInstance builds a VM parked in StateError by MarkRecoveryFailed with
// the given reason, which is how the retry decides eligibility.
func erroredInstance(id, reason string) *VM {
	return &VM{
		ID:           id,
		Status:       StateError,
		InstanceType: "t3.micro",
		Instance: &ec2.Instance{
			StateReason: &ec2.StateReason{
				Code:    aws.String("Server.RecoveryFailed"),
				Message: aws.String(reason),
			},
		},
	}
}

func TestRetryRecoveryFailed(t *testing.T) {
	t.Run("relaunches an instance failed by an unavailable backing store", func(t *testing.T) {
		m, _, _, _ := relaunchTestManager(t)
		m.deps.BackingStoreReady = func() bool { return true }

		origRun := runForRelaunch
		t.Cleanup(func() { runForRelaunch = origRun })
		var calls atomic.Int64
		runForRelaunch = func(_ *Manager, _ context.Context, _ *VM) error {
			calls.Add(1)
			return nil
		}

		inst := erroredInstance("i-retry-me", "recovery_mount_state_unavailable")
		m.Insert(inst)

		attempted := m.RetryRecoveryFailed()

		assert.Equal(t, 1, attempted, "the instance is eligible and must be retried")
		assert.EqualValues(t, 1, calls.Load(), "Run must be called for the retried instance")
		assert.NotEqual(t, StateError, m.Status(inst),
			"a successful retry must take the instance out of StateError")
	})

	t.Run("does nothing while the backing store is still down", func(t *testing.T) {
		m, _, _, _ := relaunchTestManager(t)
		m.deps.BackingStoreReady = func() bool { return false }

		origRun := runForRelaunch
		t.Cleanup(func() { runForRelaunch = origRun })
		var calls atomic.Int64
		runForRelaunch = func(_ *Manager, _ context.Context, _ *VM) error {
			calls.Add(1)
			return nil
		}

		inst := erroredInstance("i-store-down", "recovery_launch_failed")
		m.Insert(inst)

		assert.Zero(t, m.RetryRecoveryFailed(),
			"retrying against a store that is still down is how the instance got here")
		assert.Zero(t, calls.Load(), "Run must not be called")
		assert.Equal(t, StateError, m.Status(inst), "the instance must stay parked")
	})

	t.Run("leaves failures that are not dependency-related alone", func(t *testing.T) {
		m, _, _, _ := relaunchTestManager(t)
		m.deps.BackingStoreReady = func() bool { return true }

		origRun := runForRelaunch
		t.Cleanup(func() { runForRelaunch = origRun })
		var calls atomic.Int64
		runForRelaunch = func(_ *Manager, _ context.Context, _ *VM) error {
			calls.Add(1)
			return nil
		}

		// reconnect_failed and pre_relaunch_hook_failed can both mean the
		// guest itself needs looking at. Relaunching those on a timer would
		// hide the problem rather than fix it.
		for _, reason := range []string{"reconnect_failed", "pre_relaunch_hook_failed", ""} {
			m.Insert(erroredInstance("i-"+reason+"-x", reason))
		}

		// An entry that failed before its EC2 view was built has no reason at
		// all. That is ordinary, not a crash.
		m.Insert(&VM{ID: "i-no-ec2-view", Status: StateError})

		assert.Zero(t, m.RetryRecoveryFailed(),
			"only dependency-unavailable reasons are retried automatically")
		assert.Zero(t, calls.Load())
		assert.False(t, m.hasRetryableRecoveryFailures())
	})

	t.Run("gives up after the retry budget and leaves the instance for an operator", func(t *testing.T) {
		m, _, _, _ := relaunchTestManager(t)
		m.deps.BackingStoreReady = func() bool { return true }

		origRun := runForRelaunch
		t.Cleanup(func() { runForRelaunch = origRun })
		// Not ErrMountRetryable, so relaunchWithRetry returns on the first
		// attempt and every pass re-parks the instance in StateError.
		runForRelaunch = func(_ *Manager, _ context.Context, _ *VM) error {
			return assert.AnError
		}

		inst := erroredInstance("i-never-starts", "recovery_launch_failed")
		m.Insert(inst)

		for pass := 1; pass <= recoveryRetryMaxPasses; pass++ {
			require.Equal(t, 1, m.RetryRecoveryFailed(),
				"pass %d is within budget and must still be attempted", pass)
			require.Equal(t, StateError, m.Status(inst),
				"a permanently failing instance must be re-parked each pass")
		}

		assert.Zero(t, m.RetryRecoveryFailed(),
			"the budget is exhausted, so the instance is left for an operator rather than relaunched forever")
		assert.False(t, m.hasRetryableRecoveryFailures(),
			"an instance past its budget must not keep the retry loop alive")
	})

	t.Run("running and stopped instances are never touched", func(t *testing.T) {
		m, _, _, _ := relaunchTestManager(t)
		m.deps.BackingStoreReady = func() bool { return true }

		origRun := runForRelaunch
		t.Cleanup(func() { runForRelaunch = origRun })
		var calls atomic.Int64
		runForRelaunch = func(_ *Manager, _ context.Context, _ *VM) error {
			calls.Add(1)
			return nil
		}

		for _, st := range []InstanceState{StateRunning, StateStopped, StatePending, StateTerminated} {
			m.Insert(&VM{ID: "i-" + string(st), Status: st, Instance: &ec2.Instance{}})
		}

		assert.Zero(t, m.RetryRecoveryFailed(), "only StateError instances are candidates")
		assert.Zero(t, calls.Load())
	})
}

func TestRunRecoveryRetryLoop(t *testing.T) {
	t.Run("returns as soon as there is nothing left to retry", func(t *testing.T) {
		m, _, _, _ := relaunchTestManager(t)
		m.deps.BackingStoreReady = func() bool { return true }

		origInterval := recoveryRetryInterval
		recoveryRetryInterval = time.Millisecond
		t.Cleanup(func() { recoveryRetryInterval = origInterval })

		origRun := runForRelaunch
		t.Cleanup(func() { runForRelaunch = origRun })
		runForRelaunch = func(_ *Manager, _ context.Context, _ *VM) error { return nil }

		m.Insert(erroredInstance("i-recovers", "recovery_mount_state_unavailable"))

		done := make(chan struct{})
		go func() { m.RunRecoveryRetryLoop(nil); close(done) }()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("loop did not return once the backlog was cleared")
		}
	})

	t.Run("aborts promptly on shutdown instead of sleeping out its passes", func(t *testing.T) {
		m, _, _, _ := relaunchTestManager(t)
		m.deps.BackingStoreReady = func() bool { return true }

		origInterval := recoveryRetryInterval
		recoveryRetryInterval = time.Hour // a pass that sleeps would hang the test
		t.Cleanup(func() { recoveryRetryInterval = origInterval })

		origRun := runForRelaunch
		t.Cleanup(func() { runForRelaunch = origRun })
		var calls atomic.Int64
		runForRelaunch = func(_ *Manager, _ context.Context, _ *VM) error {
			calls.Add(1)
			return assert.AnError
		}

		m.Insert(erroredInstance("i-shutting-down", "recovery_launch_failed"))

		done := make(chan struct{})
		go func() { m.RunRecoveryRetryLoop(func() bool { return true }); close(done) }()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("shutdown must abort the loop before it sleeps")
		}
		assert.Zero(t, calls.Load(), "no relaunch may start once shutdown is signalled")
	})
}
