package vm

import (
	"log/slog"
	"time"
)

// Recovery-failed instances used to stay in StateError until an operator
// intervened, on the reasoning that a failed relaunch needs a human. That holds
// when the relaunch failed on its merits. It does not hold when it failed
// because a dependency was still starting, which is the common case on a
// coordinated cluster update: the daemon comes back before JetStream is serving,
// the mount cannot claim its lease, and a guest that needed a few more seconds
// is parked indefinitely.
//
// So the retry below is deliberately narrow. It reconsiders only the failures a
// not-yet-ready backing store produces, only once the store is confirmed ready,
// and only a bounded number of times — then it stops and leaves the instance for
// an operator, which is the right answer for a failure that is actually real.

// retryableRecoveryReasons are the MarkRecoveryFailed reasons that describe a
// dependency being unavailable rather than an instance being unlaunchable.
// Reasons absent from this set are never retried automatically: reconnect_failed
// and pre_relaunch_hook_failed can both indicate a guest whose state needs
// looking at, and relaunching those on a timer would hide the problem.
var retryableRecoveryReasons = map[string]bool{
	"recovery_mount_state_unavailable": true,
	"recovery_launch_failed":           true,
}

// recoveryRetryMaxPasses bounds how many times the loop reconsiders a given
// instance. Five passes at the interval below is a few minutes of trying, which
// covers a slow cluster start without turning a genuinely broken instance into a
// relaunch loop that never stops.
const recoveryRetryMaxPasses = 5

// recoveryRetryInterval is the gap between passes. Package var so tests do not
// sleep real minutes.
var recoveryRetryInterval = 30 * time.Second

// RetryRecoveryFailed reconsiders instances parked in StateError by a
// dependency-unavailable failure and relaunches them. It is a no-op unless the
// backing store is ready, because retrying against a store that is still down is
// how they got here.
//
// Returns the number of instances it attempted, so a caller can stop polling
// once there is nothing left to do.
func (m *Manager) RetryRecoveryFailed() int {
	if !m.backingStoreReady() {
		slog.Debug("recovery retry: backing store not ready, deferring")
		return 0
	}

	var toRetry []*VM

	for _, instance := range m.Snapshot() {
		if m.Status(instance) != StateError {
			continue
		}

		reason, passes := recoveryFailureReason(instance)
		if !retryableRecoveryReasons[reason] {
			continue
		}
		if passes >= recoveryRetryMaxPasses {
			continue
		}

		// StateError has no outgoing edge in the transition map, so this
		// mirrors what classifyRestoredInstances already does for a
		// drain-stopped instance: assign the field directly rather than
		// widening the state machine for every other caller.
		m.UpdateState(instance.ID, func(v *VM) {
			v.Status = StatePending
			v.recoveryRetries++
		})

		slog.Info("Retrying recovery-failed instance now the backing store is ready",
			"instanceId", instance.ID, "reason", reason, "pass", passes+1)
		toRetry = append(toRetry, instance)
	}

	if len(toRetry) == 0 {
		return 0
	}

	m.relaunchAll(toRetry)
	return len(toRetry)
}

// recoveryFailureReason returns the MarkRecoveryFailed reason recorded on the
// instance and how many automatic retries it has already had.
// A VM with no Instance has no recorded reason, so it is never retryable: the
// nil is normal for an entry that failed before its EC2 view was built.
func recoveryFailureReason(instance *VM) (string, int) {
	var reason string
	if instance.Instance != nil && instance.Instance.StateReason != nil &&
		instance.Instance.StateReason.Message != nil {
		reason = *instance.Instance.StateReason.Message
	}
	return reason, instance.recoveryRetries
}

// RunRecoveryRetryLoop retries dependency-failed instances until none are left
// or the passes are exhausted, then returns. It is not a permanent background
// loop: once the store is up and the backlog is cleared there is nothing for it
// to do, and a loop that runs forever is one more thing to reason about during
// an incident.
//
// stop is checked before every pass so a coordinated shutdown is never delayed.
func (m *Manager) RunRecoveryRetryLoop(stop func() bool) {
	for pass := 1; pass <= recoveryRetryMaxPasses; pass++ {
		if stop != nil && stop() {
			slog.Info("recovery retry loop: aborted by shutdown")
			return
		}

		if n := m.RetryRecoveryFailed(); n > 0 {
			slog.Info("recovery retry pass complete", "pass", pass, "attempted", n)
		}

		if !m.hasRetryableRecoveryFailures() {
			slog.Debug("recovery retry loop: nothing left to retry")
			return
		}

		if stop != nil && stop() {
			return
		}
		time.Sleep(recoveryRetryInterval)
	}

	if m.hasRetryableRecoveryFailures() {
		slog.Warn("recovery retry loop: giving up, instances remain in error for operator action",
			"maxPasses", recoveryRetryMaxPasses)
	}
}

// hasRetryableRecoveryFailures reports whether any instance is still parked in
// StateError with a retryable reason and retries remaining.
func (m *Manager) hasRetryableRecoveryFailures() bool {
	for _, instance := range m.Snapshot() {
		if m.Status(instance) != StateError {
			continue
		}
		reason, passes := recoveryFailureReason(instance)
		if retryableRecoveryReasons[reason] && passes < recoveryRetryMaxPasses {
			return true
		}
	}
	return false
}
