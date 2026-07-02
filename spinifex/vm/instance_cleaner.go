package vm

// InstanceCleaner releases per-instance external resources (volumes, mgmt TAP/IP,
// public IP, ENI, placement group) that the manager does not own. Invoked from
// Stop/Terminate after QEMU shuts down; methods are best-effort and self-logging.
type InstanceCleaner interface {
	// DeleteVolumes deletes the EFI internal volume via ebs.delete and user
	// volumes flagged DeleteOnTermination via the volume service.
	// Called only on Terminate, never on Stop. Returns the first delete error.
	DeleteVolumes(v *VM) error

	// CleanupMgmtNetwork removes the management TAP device and releases the
	// management IP allocation. Called on both Stop and Terminate.
	CleanupMgmtNetwork(v *VM)

	// ReleasePublicIP publishes vpc.delete-nat and releases the public IP
	// back to the external IPAM pool. Called only on Terminate when the
	// instance has a public IP allocated. Returns the IPAM release error.
	ReleasePublicIP(v *VM) error

	// DetachAndDeleteENI detaches and force-deletes the primary ENI (bypassing
	// the in-use guard for the owning instance). NotFound is tolerated. Called
	// only on Terminate. Returns the delete error.
	DetachAndDeleteENI(v *VM) error

	// RemoveFromPlacementGroup removes the instance from its placement
	// group, if one is set. Called only on Terminate.
	RemoveFromPlacementGroup(v *VM) error

	// RemoveFromSpotRequest closes the Spot Instance Request fulfilled by this
	// instance, if any, by scanning the active spot bucket for its ID. The VM
	// carries no spot marker, so this is best-effort and untracked (no teardown
	// stamp): a no-match is the common case. Called only on Terminate.
	RemoveFromSpotRequest(v *VM) error

	// ReleaseGPU unbinds the instance's GPU from vfio-pci and rebinds to
	// its original host driver. Called on both Stop and Terminate. No-op
	// when the instance has no GPU allocation.
	ReleaseGPU(v *VM) error
}
