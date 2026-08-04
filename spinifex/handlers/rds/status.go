package handlers_rds

import "slices"

// The only instance state the API exposes, derived from the VM, data volume and
// agent heartbeat, so a customer never depends on a DB instance being a VM.
type Status string

const (
	// Provisioning through to the first healthy heartbeat.
	StatusCreating  Status = "creating"
	StatusAvailable Status = "available"
	StatusModifying Status = "modifying"
	StatusBackingUp Status = "backing-up"
	// Also when parameters marked pending-reboot are applied.
	StatusRebooting Status = "rebooting"
	StatusStopping  Status = "stopping"
	StatusStarting  Status = "starting"
	// No VM; the data volume and ENI are retained.
	StatusStopped Status = "stopped"
	// Reconciler-driven auto-recovery onto a fresh VM.
	StatusRecovering Status = "recovering"
	// Teardown, including the optional final snapshot.
	StatusDeleting Status = "deleting"
	// Terminal; the record is removed once observed.
	StatusDeleted Status = "deleted"
	StatusFailed  Status = "failed"
)

// Every non-terminal state can reach failed and deleting, since a delete is
// accepted at any point and any step can fail.
var transitions = map[Status][]Status{
	StatusCreating:  {StatusAvailable, StatusFailed, StatusDeleting},
	StatusAvailable: {StatusModifying, StatusBackingUp, StatusRebooting, StatusStopping, StatusRecovering, StatusFailed, StatusDeleting},
	StatusModifying: {StatusAvailable, StatusFailed, StatusDeleting},
	// A snapshot of a stopped instance needs no quiesce and leaves it stopped, so
	// backing-up has to be reachable from and returnable to both settled states.
	StatusBackingUp:  {StatusAvailable, StatusStopped, StatusFailed, StatusDeleting},
	StatusRebooting:  {StatusAvailable, StatusFailed, StatusDeleting},
	StatusStopping:   {StatusStopped, StatusFailed, StatusDeleting},
	StatusStopped:    {StatusStarting, StatusModifying, StatusBackingUp, StatusFailed, StatusDeleting},
	StatusStarting:   {StatusAvailable, StatusFailed, StatusDeleting},
	StatusRecovering: {StatusAvailable, StatusFailed, StatusDeleting},
	// A failed instance is retried by the reconciler rather than being stuck:
	// recovering is the reconciler's path back, available the heartbeat's, and
	// modifying the customer's own retry of the change that failed.
	StatusFailed:   {StatusRecovering, StatusAvailable, StatusModifying, StatusDeleting},
	StatusDeleting: {StatusDeleted, StatusFailed},
	StatusDeleted:  nil,
}

// Statuses meaning the control plane is already acting on this instance, so the
// recovery classifier does not recover a VM out from under a lifecycle op.
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

// Anything read back from KV that fails this check was written by a newer or
// corrupted control plane, not a state to act on.
func (s Status) Valid() bool {
	_, ok := transitions[s]
	return ok
}

func (s Status) Terminal() bool {
	return s == StatusDeleted
}

func (s Status) Transitional() bool {
	return transitional[s]
}

// A no-op is legal for any valid status, so a repeated observation of the same
// state is not treated as an illegal transition.
func CanTransition(from, to Status) bool {
	if !from.Valid() || !to.Valid() {
		return false
	}
	if from == to {
		return true
	}
	return slices.Contains(transitions[from], to)
}

// The snapshot's own lifecycle, separate from the instance's backing-up state
// The instance is derived from an in-flight control-plane operation, while
// the snapshot record moves creating → available once the data exists. There is
// no failed state — a snapshot that never completed is removed rather than kept
// as an unrestorable record.
var snapshotTransitions = map[string][]string{
	SnapshotStatusCreating:  {SnapshotStatusAvailable},
	SnapshotStatusAvailable: nil,
}

func ValidSnapshotStatus(status string) bool {
	_, ok := snapshotTransitions[status]
	return ok
}

func CanTransitionSnapshot(from, to string) bool {
	if !ValidSnapshotStatus(from) || !ValidSnapshotStatus(to) {
		return false
	}
	if from == to {
		return true
	}
	return slices.Contains(snapshotTransitions[from], to)
}
