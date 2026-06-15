package daemon

import (
	"fmt"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/vm"
)

// TransitionState validates and applies a state transition on the given instance.
// Returns vm.ErrInvalidTransition on bad transitions; WriteState failure is also
// returned but in-memory status is kept since the VM has already changed state.
func (d *Daemon) TransitionState(instance *vm.VM, target vm.InstanceState) error {
	var (
		current vm.InstanceState
		invalid bool
	)
	// Inspect (not UpdateState): MarkFailed may invoke this for an instance
	// that is no longer in the running map.
	d.vmMgr.Inspect(instance, func(v *vm.VM) {
		current = v.Status
		if !vm.IsValidTransition(current, target) {
			invalid = true
			return
		}
		v.Status = target
	})
	if invalid {
		return fmt.Errorf("%w: %s -> %s for instance %s",
			vm.ErrInvalidTransition, current, target, instance.ID)
	}

	slog.Info("Instance state transition", "instanceId", instance.ID, "from", string(current), "to", string(target))

	if err := d.WriteState(); err != nil {
		slog.Error("Failed to persist state after transition", "instanceId", instance.ID,
			"from", string(current), "to", string(target), "err", err)
		return fmt.Errorf("state transition applied but write failed: %w", err)
	}
	return nil
}
