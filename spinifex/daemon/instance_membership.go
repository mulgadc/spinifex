package daemon

import "github.com/mulgadc/spinifex/spinifex/vm"

// One key now holds an instance for its whole life, so which set it belongs to
// is a predicate over the record rather than the prefix it sits under. Before
// the cutover it was structural — a stopped instance sat at instance.<id> and a
// running one inside node.<nodeID> — and could not be wrong. It can be now, so
// it is decided here once instead of at each call site.

// operatorStopped reports whether a record is stopped in the sense
// DescribeStoppedInstances means: stopped because someone asked for it.
//
// Status alone will not do. A node's DRAIN sequence also leaves an instance in
// StateStopped, and restore relaunches those rather than listing them.
// DesiredState is what separates the two, and this is the same test
// classifyRestoredInstances already makes.
func operatorStopped(record *vm.InstanceRecord) bool {
	if record == nil {
		return false
	}
	return record.Status.Status == vm.StateStopped &&
		record.Spec.DesiredState == vm.DesiredStopped
}

// runsOn reports whether a record belongs to nodeID's running set: everything
// that node last owned except what an operator stopped.
//
// StateTerminated is deliberately included. Restore migrates those to the
// terminated bucket, and a record it cannot see is one it cannot migrate —
// which is the "void" its own comment warns about.
func runsOn(record *vm.InstanceRecord, nodeID string) bool {
	if record == nil || nodeID == "" {
		return false
	}
	return record.Status.LastNode == nodeID && !operatorStopped(record)
}

// recoverable reports whether a record is one another node may take over when
// the node named on it stops heartbeating.
//
// Desired state is the test, not observed state. A node that dies hard leaves
// its instances observed-running and never writes anything again, and a node
// drained for maintenance leaves them observed-stopped but still wanted — both
// are recoveries. What is excluded is an instance nobody wants running: an
// operator stop, or a terminate already under way.
//
// A fenced instance is excluded for the same reason the restore path excludes
// it. The fence does not seal, so no node can show the volumes are mountable,
// and taking it over is how an unproven state becomes a destroyed one.
func recoverable(record *vm.InstanceRecord) bool {
	if record == nil || record.Spec.DesiredState != vm.DesiredRunning {
		return false
	}
	if record.Metadata.MarkedForDeletion() {
		return false
	}
	switch record.Status.Status {
	case vm.StateTerminated, vm.StateShuttingDown:
		return false
	}
	if vm.VolumeFenced(record.Status.Instance) {
		return false
	}
	return !storageFaulted(record)
}

// storageFaulted reports whether the last health an instance published was its
// storage refusing I/O, which werror=stop turns into a paused guest.
//
// This is the failure another node cannot fix. The volumes are the same objects
// in the same object store from anywhere, so a backend refusing them refuses
// them everywhere, and relaunching elsewhere moves the pause rather than ending
// it. Left alone the guest keeps its held request and resumes when the backend
// returns, which is the better outcome than a relaunch that loses its RAM and
// then pauses too.
func storageFaulted(record *vm.InstanceRecord) bool {
	return !record.Status.Health.IOErrorSince.IsZero()
}
