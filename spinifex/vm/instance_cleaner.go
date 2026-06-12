package vm

// InstanceCleaner releases per-instance external resources (volumes, mgmt TAP/IP,
// public IP, ENI, placement group) that the manager does not own. Invoked from
// Stop/Terminate after QEMU shuts down; methods are best-effort and self-logging.
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
