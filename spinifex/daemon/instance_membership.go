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
