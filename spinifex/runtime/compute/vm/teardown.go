package vm

// TeardownState is the per-dependent teardown progress recorded on a VM's KV
// record during terminate. A record is fully reapable only once every tracked
// dependent is TeardownDone; pending/failed entries are completed by the GC
// backstop (ADR-0003 §1).
type TeardownState string

const (
	TeardownDone    TeardownState = "done"
	TeardownPending TeardownState = "pending"
	TeardownFailed  TeardownState = "failed"
)

// Teardown dependent keys. OVN and NAT are fire-and-forget publishes recorded
// as pending; the reconcile LSP prune / drift NAT heal complete them.
const (
	TeardownQEMU      = "qemu"
	TeardownVolumes   = "volumes"
	TeardownTap       = "tap"
	TeardownGPU       = "gpu"
	TeardownNAT       = "nat"
	TeardownENI       = "eni"
	TeardownOVN       = "ovn"
	TeardownPlacement = "placement"
)

// markTeardown records dep → state on the VM under the manager lock. Inspect
// (not UpdateState) so it also works for a not-yet-inserted instance.
func (m *Manager) markTeardown(instance *VM, dep string, state TeardownState) {
	m.Inspect(instance, func(v *VM) {
		if v.Teardown == nil {
			v.Teardown = make(map[string]string)
		}
		v.Teardown[dep] = string(state)
	})
}

// markTeardownResult records done on nil error, failed otherwise.
func (m *Manager) markTeardownResult(instance *VM, dep string, err error) {
	m.markTeardown(instance, dep, resultState(err))
}

// resultState maps a cleaner call's error into its teardown mark: done on
// nil, failed otherwise.
func resultState(err error) TeardownState {
	if err != nil {
		return TeardownFailed
	}
	return TeardownDone
}

// TeardownComplete reports whether every tracked dependent is done. A terminated
// record with any pending/failed dependent is retained (GC-visible).
func (v *VM) TeardownComplete() bool {
	for _, state := range v.Teardown {
		if TeardownState(state) != TeardownDone {
			return false
		}
	}
	return true
}
