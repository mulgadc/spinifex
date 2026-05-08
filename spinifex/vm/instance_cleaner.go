package vm

// InstanceCleaner releases the per-instance external resources that the
// manager itself does not own: storage volumes, the management TAP/IP, the
// public IP allocation, the primary ENI, and placement-group membership.
// The manager invokes these from Stop/Terminate after QEMU has shut down
// and volumes have been unmounted; methods are best-effort and log their
// own errors.
//
// The live implementation lives in the daemon package and delegates to the
// existing volume / VPC / EIP / placement-group services. Tests inject a
// recording fake.
type InstanceCleaner interface {
	// DeleteVolumes deletes EFI / cloud-init internal volumes via ebs.delete
	// and user volumes flagged DeleteOnTermination via the volume service.
	// Called only on Terminate, never on Stop.
	DeleteVolumes(v *VM)

	// CleanupMgmtNetwork removes the management TAP device and releases the
	// management IP allocation. Called on both Stop and Terminate.
	CleanupMgmtNetwork(v *VM)

	// ReleasePublicIP publishes vpc.delete-nat and releases the public IP
	// back to the external IPAM pool. Called only on Terminate when the
	// instance has a public IP allocated.
	ReleasePublicIP(v *VM)

	// DetachAndDeleteENI detaches and deletes the primary ENI. NotFound is
	// tolerated. Called only on Terminate.
	DetachAndDeleteENI(v *VM)

	// RemoveFromPlacementGroup removes the instance from its placement
	// group, if one is set. Called only on Terminate.
	RemoveFromPlacementGroup(v *VM)

	// ReleaseGPU unbinds the instance's GPU from vfio-pci and rebinds to
	// its original host driver. Called on both Stop and Terminate. No-op
	// when the instance has no GPU allocation.
	ReleaseGPU(v *VM)
}
