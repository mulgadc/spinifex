package vm

// QEMUExitProven reports whether the instance's QEMU is proven gone by this
// node's PID file. A completed QEMU teardown stamp is not proof: the terminate
// path stamps it even when the kill failed. It is only meaningful on the home node.
func QEMUExitProven(v *VM) bool {
	return qemuProvenGone(v.ID)
}

// LeakedVolumeInstances returns the instances this node owns that were
// terminated with a failed volumes teardown and whose QEMU is proven gone. A
// volume attached to an instance whose QEMU may still run is not leaked, so
// nothing may free it yet. The home-node check runs first so a PID file is never
// judged on another node.
func LeakedVolumeInstances(terminated []*VM, node string) map[string]bool {
	leaked := make(map[string]bool)
	for _, v := range terminated {
		if v.LastNode != node || TeardownState(v.Teardown[TeardownVolumes]) != TeardownFailed {
			continue
		}
		if QEMUExitProven(v) {
			leaked[v.ID] = true
		}
	}
	return leaked
}
