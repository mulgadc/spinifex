package handlers_rds

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestStatus_Valid(t *testing.T) {
	t.Parallel()
	for _, s := range []Status{
		StatusCreating, StatusAvailable, StatusModifying, StatusBackingUp,
		StatusRebooting, StatusStopping, StatusStarting, StatusStopped,
		StatusRecovering, StatusDeleting, StatusDeleted, StatusFailed,
	} {
		assert.True(t, s.Valid(), "status %q should be valid", s)
	}

	// The underlying EC2 states must never leak through as DB instance states.
	for _, s := range []Status{"", "running", "pending", "terminated", "Available"} {
		assert.False(t, s.Valid(), "status %q should not be valid", s)
	}
}

func TestCanTransition_Lifecycle(t *testing.T) {
	t.Parallel()
	tests := []struct {
		from, to Status
		want     bool
	}{
		// The happy path through create, use, stop, start and delete.
		{StatusCreating, StatusAvailable, true},
		{StatusAvailable, StatusBackingUp, true},
		{StatusBackingUp, StatusAvailable, true},
		{StatusAvailable, StatusStopping, true},
		{StatusStopping, StatusStopped, true},
		{StatusStopped, StatusStarting, true},
		{StatusStarting, StatusAvailable, true},
		{StatusAvailable, StatusDeleting, true},
		{StatusDeleting, StatusDeleted, true},

		// Failure and self-heal.
		{StatusAvailable, StatusRecovering, true},
		{StatusAvailable, StatusFailed, true},
		{StatusFailed, StatusRecovering, true},
		// A failed instance is exactly the one a customer retries a change on.
		{StatusFailed, StatusModifying, true},
		{StatusAvailable, StatusModifying, true},
		{StatusModifying, StatusAvailable, true},
		{StatusRecovering, StatusAvailable, true},

		// A re-observation of the same state is not an illegal move.
		{StatusAvailable, StatusAvailable, true},
		{StatusDeleted, StatusDeleted, true},

		// A stopped instance has no VM to reboot, but its datadir was sealed by the
		// stop, which is exactly what a snapshot wants to read.
		{StatusStopped, StatusBackingUp, true},
		{StatusStopped, StatusRebooting, false},
		{StatusStopped, StatusAvailable, false},

		// Deletion is one-way: a deleted instance is never resurrected, and a
		// delete in flight is never walked back into service.
		{StatusDeleted, StatusAvailable, false},
		{StatusDeleted, StatusCreating, false},
		{StatusDeleting, StatusAvailable, false},

		// Creation never lands straight in a steady non-available state.
		{StatusCreating, StatusStopped, false},

		// Unknown states resolve to no legal transition at all.
		{StatusAvailable, "running", false},
		{"running", StatusAvailable, false},
		{"running", "running", false},
	}

	for _, tc := range tests {
		assert.Equal(t, tc.want, CanTransition(tc.from, tc.to),
			"CanTransition(%q, %q)", tc.from, tc.to)
	}
}

// Every state the machine can reach must itself be a state the machine knows,
// or the reconciler could write a status it then refuses to read back.
func TestTransitions_TargetsAreValidStates(t *testing.T) {
	t.Parallel()
	for from, targets := range transitions {
		assert.True(t, from.Valid(), "source status %q should be valid", from)
		for _, to := range targets {
			assert.True(t, to.Valid(), "target status %q of %q should be valid", to, from)
		}
	}
}
