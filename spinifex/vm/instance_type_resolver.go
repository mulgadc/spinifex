package vm

// InstanceTypeSpec carries the QEMU-relevant numbers for an instance type
// (vCPU/memory/architecture) without dragging the EC2 SDK type into the vm
// package.
type InstanceTypeSpec struct {
	VCPUs        int
	MemoryMiB    int
	Architecture string
}

// InstanceTypeResolver returns the spec for an instance type name (e.g.
// "t3.micro"). Returns ok=false when the type is unknown to the host.
type InstanceTypeResolver interface {
	Resolve(name string) (spec InstanceTypeSpec, ok bool)
}

// ResourceController reserves and releases capacity for an instance type.
// Allocation is keyed by name so callers don't need to round-trip the EC2
// SDK type. The real implementation is the daemon's ResourceManager.
type ResourceController interface {
	Allocate(instanceType string) error
	Deallocate(instanceType string)
	// ReleaseToReservation returns instanceType's compute to the capacity
	// reservation reservationID instead of the general pool — the net-zero
	// inverse of the launch-time swap, used when a reservation-bound instance
	// stops or terminates. Frees to the general pool if the reservation is gone.
	ReleaseToReservation(reservationID, instanceType string)
	// CanAllocate returns how many instances of instanceType could be
	// allocated right now (0 to count). Used by the crash-recovery
	// scheduler to avoid restarting a VM the host can't fit.
	CanAllocate(instanceType string, count int) int
}

// VolumeStateUpdater is the narrow slice of the volume service the manager
// touches: marking boot volumes "in-use" once a VM is confirmed running.
type VolumeStateUpdater interface {
	UpdateVolumeState(volumeID, state, instanceID, attachmentDevice string) error
}
