package handlers_rds

import "slices"

// Status is a DB instance lifecycle state (rds-v1.md D4). It is the only
// instance state the API exposes: the control plane derives it from the
// underlying VM, data volume and agent heartbeat, and never surfaces the EC2
// instance state directly, so a customer cannot come to depend on the fact that
// a DB instance happens to be a VM.
type Status string

const (
	// StatusCreating covers provisioning through to the first healthy heartbeat.
	StatusCreating Status = "creating"
	// StatusAvailable is the steady state: engine up, heartbeat fresh.
	StatusAvailable Status = "available"
	// StatusModifying covers an in-flight ModifyDBInstance, including the ones
	// that stop, alter and restart the VM.
	StatusModifying Status = "modifying"
	// StatusBackingUp covers a manual or automated snapshot of the data volume.
	StatusBackingUp Status = "backing-up"
	// StatusRebooting covers RebootDBInstance, which is also when parameters
	// marked pending-reboot are applied.
	StatusRebooting Status = "rebooting"
	// StatusStopping covers StopDBInstance up to the VM being down.
	StatusStopping Status = "stopping"
	// StatusStarting covers StartDBInstance up to the engine being reachable.
	StatusStarting Status = "starting"
	// StatusStopped is a stopped instance: no VM, data volume and ENI retained.
	StatusStopped Status = "stopped"
	// StatusRecovering covers reconciler-driven auto-recovery onto a fresh VM.
	StatusRecovering Status = "recovering"
	// StatusDeleting covers teardown, including the optional final snapshot.
	StatusDeleting Status = "deleting"
	// StatusDeleted is terminal; the record is removed once observed.
	StatusDeleted Status = "deleted"
	// StatusFailed is an instance the control plane could not keep healthy and
	// has not yet recovered.
	StatusFailed Status = "failed"
)

// transitions is the D4 state machine: for each status, the states it may move
// to. Every non-terminal state can reach failed and deleting, because a delete
// is accepted at any point and any step can fail; the interesting content is
// which lifecycle state each transitional status settles back into.
var transitions = map[Status][]Status{
	StatusCreating:   {StatusAvailable, StatusFailed, StatusDeleting},
	StatusAvailable:  {StatusModifying, StatusBackingUp, StatusRebooting, StatusStopping, StatusRecovering, StatusFailed, StatusDeleting},
	StatusModifying:  {StatusAvailable, StatusFailed, StatusDeleting},
	StatusBackingUp:  {StatusAvailable, StatusFailed, StatusDeleting},
	StatusRebooting:  {StatusAvailable, StatusFailed, StatusDeleting},
	StatusStopping:   {StatusStopped, StatusFailed, StatusDeleting},
	StatusStopped:    {StatusStarting, StatusModifying, StatusFailed, StatusDeleting},
	StatusStarting:   {StatusAvailable, StatusFailed, StatusDeleting},
	StatusRecovering: {StatusAvailable, StatusFailed, StatusDeleting},
	// A failed instance is retried by the reconciler rather than being stuck:
	// recovering is the reconciler's path back, available the heartbeat's.
	StatusFailed:   {StatusRecovering, StatusAvailable, StatusDeleting},
	StatusDeleting: {StatusDeleted, StatusFailed},
	StatusDeleted:  nil,
}

// transitional lists the statuses that mean "the control plane is already acting
// on this instance". The recovery classifier consults it so a VM that is down
// because a lifecycle operation took it down is not mistaken for a failure and
// recovered out from under the operation driving it.
var transitional = map[Status]bool{
	StatusCreating:   true,
	StatusModifying:  true,
	StatusBackingUp:  true,
	StatusRebooting:  true,
	StatusStopping:   true,
	StatusStarting:   true,
	StatusRecovering: true,
	StatusDeleting:   true,
}

// Valid reports whether s is a status this control plane can produce. Anything
// read back from KV that fails this check is a record written by a newer or
// corrupted control plane, not a state to act on.
func (s Status) Valid() bool {
	_, ok := transitions[s]
	return ok
}

// Terminal reports whether s admits no further transition.
func (s Status) Terminal() bool {
	return s == StatusDeleted
}

// Transitional reports whether s is a state the control plane is actively
// driving, as opposed to a settled one (available, stopped, failed, deleted).
func (s Status) Transitional() bool {
	return transitional[s]
}

// CanTransition reports whether from → to is a legal move in the D4 lifecycle.
// A no-op (from == to) is legal for any valid status so a repeated observation
// of the same state is not treated as an illegal transition.
func CanTransition(from, to Status) bool {
	if !from.Valid() || !to.Valid() {
		return false
	}
	if from == to {
		return true
	}
	return slices.Contains(transitions[from], to)
}
