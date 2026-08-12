package cmd

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
)

// firewallApplyHelper is the fixed-verb root helper setup.sh installs. Absent on
// a developer machine and on any node installed with --firewall=off, where there
// is no policy to punch a hole in.
var firewallApplyHelper = "/usr/local/lib/spinifex/spinifex-firewall-apply"

// openFormationPort opens the formation port to any source and returns the
// function that closes it again.
//
// The port is peer-scoped in the steady-state policy, where it carries the
// daemon cluster manager. A node dialling in to join is not a peer yet, so
// without this the join handshake is dropped rather than refused and the joiner
// retries in silence until it times out. The window is bounded by formation
// itself, and the handshake behind it is TLS 1.3 with a bearer token.
//
// Best effort: a node with no policy installed needs nothing done, and a
// failure to open must not stop an operator forming a cluster.
func openFormationPort(port int) func() {
	if _, err := os.Stat(firewallApplyHelper); err != nil {
		return func() {}
	}

	if err := runFirewallHelper("open-port", port); err != nil {
		fmt.Printf("⚠️  Could not open port %d in the host firewall: %v\n", port, err)
		fmt.Printf("   Joining nodes may be unable to reach the formation server.\n")
		return func() {}
	}

	return func() {
		if err := runFirewallHelper("close-port", port); err != nil {
			// Loud, not fatal: formation has already succeeded by here, and the
			// port stays open until the next reconcile or setup.sh run.
			slog.Error("Failed to close the formation port in the host firewall",
				"port", port, "helper", firewallApplyHelper, "err", err)
		}
	}
}

func runFirewallHelper(verb string, port int) error {
	cmd := exec.Command(firewallApplyHelper, verb, strconv.Itoa(port))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w: %s", verb, err, out)
	}
	return nil
}
