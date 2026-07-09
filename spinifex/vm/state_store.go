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
	// ClaimStoppedInstance atomically removes id's record from the shared
	// store and returns the VM it held, so that at most one caller across
	// the cluster can ever win a race to (re)launch the same stopped
	// instance. Returns ErrStoppedInstanceClaimed if a concurrent caller
	// already claimed (or otherwise removed) the record.
	ClaimStoppedInstance(id string) (*VM, error)

	WriteTerminatedInstance(id string, v *VM) error
	ListTerminatedInstances() ([]*VM, error)
	DeleteTerminatedInstance(id string) error
	// UpdateTerminatedInstance atomically applies mutate to the current
	// record for id and writes it back using optimistic concurrency (CAS),
	// so a concurrent writer to the same record (e.g. two teardown-reaper
	// sweeps advancing different dependents) cannot clobber the other's
	// progress. Returns an error if no record exists for id.
	UpdateTerminatedInstance(id string, mutate func(*VM)) (*VM, error)
}
