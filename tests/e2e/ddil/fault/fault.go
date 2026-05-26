//go:build e2e

// Package fault provides DDIL-only fault-injection helpers — iptables-based
// peer partitioning, NATS/daemon lifecycle toggles, netem link-condition
// shaping, and instance-state snapshots used to assert that nothing rewound
// across a partition/heal cycle. Generic cluster/SSH/daemon primitives live
// in github.com/mulgadc/spinifex/tests/e2e/harness.
package fault

import (
	"context"
	"fmt"
	"strings"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// PartitionNode isolates target from peers by installing per-peer iptables
// DROP rules for both directions. The orchestrator's SSH source IP must not
// be one of peers (it normally isn't: the peers are cluster members, and the
// orchestrator is the CI runner). After applying, a sanity-check SSH echo
// confirms the control plane is still reachable; a lost control-plane SSH
// is a loud error rather than a silent hang.
//
// The rule set is additive (iptables -I INPUT/OUTPUT) so PartitionNode
// composes with any prior non-DDIL rules. HealNode's flush is still the
// correct teardown because the harness owns the target's firewall during a
// scenario.
func PartitionNode(ctx context.Context, ssh harness.SSH, target harness.Node, peers []harness.Node) error {
	if len(peers) == 0 {
		return fmt.Errorf("ddil fault: PartitionNode %s: no peers supplied", target.Name)
	}

	var lines []string
	for _, p := range peers {
		lines = append(lines,
			fmt.Sprintf("sudo iptables -I INPUT -s %s -j DROP", harness.ShellQuote(p.Addr)),
			fmt.Sprintf("sudo iptables -I OUTPUT -d %s -j DROP", harness.ShellQuote(p.Addr)),
		)
	}
	cmd := strings.Join(lines, " && ")
	if _, err := ssh.Run(ctx, target, cmd); err != nil {
		return fmt.Errorf("ddil fault: partition %s: %w", target.Name, err)
	}

	// Sanity-check: if our SSH path ran through a peer IP, the rules we just
	// installed would have severed it. Fail loudly so a future infra change
	// (e.g. orchestrator moved onto the cluster network) surfaces immediately.
	if _, err := ssh.Run(ctx, target, "echo ok"); err != nil {
		return fmt.Errorf("ddil fault: partition %s severed orchestrator SSH "+
			"(orchestrator may share peer IPs): %w", target.Name, err)
	}
	return nil
}

// HealNode flushes and deletes all iptables rules on target, reversing a
// prior PartitionNode. Idempotent: safe on a node with no rules installed.
func HealNode(ctx context.Context, ssh harness.SSH, target harness.Node) error {
	if _, err := ssh.Run(ctx, target, "sudo iptables -F && sudo iptables -X"); err != nil {
		return fmt.Errorf("ddil fault: heal %s: %w", target.Name, err)
	}
	return nil
}

// KillNATS stops spinifex-nats on node without touching spinifex-daemon.
// This is the scenario shape the current happy-path suite cannot express —
// `systemctl stop spinifex.target` brings both down together.
func KillNATS(ctx context.Context, ssh harness.SSH, node harness.Node) error {
	if _, err := ssh.Run(ctx, node, "sudo systemctl stop spinifex-nats"); err != nil {
		return fmt.Errorf("ddil fault: kill nats on %s: %w", node.Name, err)
	}
	return nil
}

// StartNATS starts spinifex-nats on node. Paired with KillNATS.
func StartNATS(ctx context.Context, ssh harness.SSH, node harness.Node) error {
	if _, err := ssh.Run(ctx, node, "sudo systemctl start spinifex-nats"); err != nil {
		return fmt.Errorf("ddil fault: start nats on %s: %w", node.Name, err)
	}
	return nil
}

// RestartDaemonOnly restarts spinifex-daemon without touching spinifex-nats.
// Used by Scenario B to exercise the daemon-without-NATS startup path
// (daemon-local-autonomy §1d).
func RestartDaemonOnly(ctx context.Context, ssh harness.SSH, node harness.Node) error {
	if _, err := ssh.Run(ctx, node, "sudo systemctl restart spinifex-daemon"); err != nil {
		return fmt.Errorf("ddil fault: restart daemon on %s: %w", node.Name, err)
	}
	return nil
}
