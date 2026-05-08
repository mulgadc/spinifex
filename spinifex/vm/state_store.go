package vm

// StateStore persists VM state to the per-node "running" bucket and to the
// cluster-shared "stopped" / "terminated" buckets.
//
// The interface lives in the vm package; the live implementation is a thin
// adapter in the daemon package wrapping the JetStream-backed bucket. Both
// vm.Manager and daemon-side handlers route VM-instance state I/O through
// this interface so the daemon can be exercised against an in-memory fake.
type StateStore interface {
	// SaveRunningState writes the snapshot of VMs currently running on the
	// given node. The supplied map is owned by the caller and must not be
	// retained.
	SaveRunningState(nodeID string, snapshot map[string]*VM) error
	// LoadRunningState returns the VMs persisted for the given node. An
	// empty (non-nil) map is returned when no state exists.
	LoadRunningState(nodeID string) (map[string]*VM, error)

	WriteStoppedInstance(id string, v *VM) error
	LoadStoppedInstance(id string) (*VM, error)
	DeleteStoppedInstance(id string) error
	ListStoppedInstances() ([]*VM, error)

	WriteTerminatedInstance(id string, v *VM) error
	ListTerminatedInstances() ([]*VM, error)
}
