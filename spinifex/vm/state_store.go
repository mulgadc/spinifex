package vm

// StateStore persists VM state to the per-node "running" bucket and the
// cluster-shared "stopped"/"terminated" buckets. The live implementation is a
// JetStream adapter in the daemon package; tests inject an in-memory fake.
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
