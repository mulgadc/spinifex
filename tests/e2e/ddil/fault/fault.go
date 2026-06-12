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

// PartitionNode isolates target from peers with per-peer iptables DROP rules (INPUT+OUTPUT).
// A post-partition SSH echo confirms the orchestrator's control-plane path was not severed;
// rules are additive so PartitionNode composes with existing non-DDIL firewall state.
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

	// Confirm orchestrator SSH still works; if not, the orchestrator shares a peer IP.
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

// KillNATS stops spinifex-nats without touching spinifex-daemon (unlike systemctl stop
// spinifex.target which brings both down).
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

// RestartDaemonOnly restarts spinifex-daemon without touching spinifex-nats,
// exercising the daemon-without-NATS startup path.
func RestartDaemonOnly(ctx context.Context, ssh harness.SSH, node harness.Node) error {
	if _, err := ssh.Run(ctx, node, "sudo systemctl restart spinifex-daemon"); err != nil {
		return fmt.Errorf("ddil fault: restart daemon on %s: %w", node.Name, err)
	}
	return nil
}
