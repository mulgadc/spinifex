package host

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// revalidatorLine matches one revalidator in `ovs-appctl upcall/show`, such as
// "  19: (keys 3)". ovs-vswitchd starts its handler and revalidator threads
// together, so a datapath with none listed has nothing answering its upcalls.
var revalidatorLine = regexp.MustCompile(`(?m)^\s+\d+: \(keys \d+\)`)

// VswitchdHeldInFlowRestore reports whether ovs-vswitchd has flow-restore-wait
// set and no upcall threads, so it forwards nothing. ovn-controller re-sets the
// flag on healthy nodes too, so the threads are the signal, not the flag.
func VswitchdHeldInFlowRestore(ctx context.Context, r Runner) (bool, error) {
	flag, err := r.Run(ctx, "ovs-vsctl", "--if-exists", "get", "Open_vSwitch", ".", "other_config:flow-restore-wait")
	if err != nil {
		return false, fmt.Errorf("read flow-restore-wait: %w", err)
	}
	if strings.Trim(strings.TrimSpace(string(flag)), `"`) != "true" {
		return false, nil
	}
	upcalls, err := r.Run(ctx, "ovs-appctl", "upcall/show")
	if err != nil {
		return false, fmt.Errorf("ovs-appctl upcall/show: %w", err)
	}
	return !revalidatorLine.Match(upcalls), nil
}

// ReleaseFlowRestoreWait clears flow-restore-wait so ovs-vswitchd starts its
// upcall threads. ovn-controller sets it again afterwards, which a running
// ovs-vswitchd ignores.
func ReleaseFlowRestoreWait(ctx context.Context, r Runner) error {
	if out, err := r.Run(ctx, "ovs-vsctl", "set", "Open_vSwitch", ".", "other_config:flow-restore-wait=false"); err != nil {
		return fmt.Errorf("clear flow-restore-wait: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}
