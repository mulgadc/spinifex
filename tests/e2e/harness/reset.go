//go:build e2e

package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// routezResponse is the subset of NATS /routez used to count unique peer names.
// Raw route count is not used; NATS opens multiple TCP routes per peer.
type routezResponse struct {
	Routes []struct {
		RemoteName string `json:"remote_name"`
	} `json:"routes"`
}

// ResetAllNodes returns the cluster to a known clean state: iptables flushed,
// tc netem removed, and spinifex-nats + spinifex-daemon running on every node.
// Idempotent — safe to call on a clean cluster.
func ResetAllNodes(ctx context.Context, c *Cluster, ssh SSH) error {
	for _, n := range c.Nodes {
		if err := resetNode(ctx, ssh, n); err != nil {
			return err
		}
	}
	return waitClusterHealthy(ctx, c, ssh, 2*time.Minute)
}

func resetNode(ctx context.Context, ssh SSH, n Node) error {
	// -F flushes INPUT/OUTPUT DROP rules from PartitionNode; -X removes stale user chains.
	if _, err := ssh.Run(ctx, n, "sudo iptables -F && sudo iptables -X"); err != nil {
		return fmt.Errorf("e2e harness: reset iptables on %s: %w", n.Name, err)
	}

	// Remove netem qdiscs only from interfaces that carry one; NIC names vary.
	ifaces, err := netemIfaces(ctx, ssh, n)
	if err != nil {
		return err
	}
	for _, iface := range ifaces {
		cmd := fmt.Sprintf("sudo tc qdisc del dev %s root 2>/dev/null || true", ShellQuote(iface))
		if _, err := ssh.Run(ctx, n, cmd); err != nil {
			return fmt.Errorf("e2e harness: clear netem on %s (%s): %w", n.Name, iface, err)
		}
	}

	// Start services — noop if already active (systemd returns 0).
	if _, err := ssh.Run(ctx, n, "sudo systemctl start spinifex-nats spinifex-daemon"); err != nil {
		return fmt.Errorf("e2e harness: start services on %s: %w", n.Name, err)
	}
	return nil
}

// netemIfaces lists every interface on node that has a netem root qdisc.
func netemIfaces(ctx context.Context, ssh SSH, n Node) ([]string, error) {
	out, err := ssh.Run(ctx, n, "tc qdisc show | awk '/qdisc netem/ {print $5}' | sort -u")
	if err != nil {
		return nil, fmt.Errorf("e2e harness: list netem qdiscs on %s: %w", n.Name, err)
	}
	return strings.Fields(string(out)), nil
}

func waitClusterHealthy(ctx context.Context, c *Cluster, ssh SSH, timeout time.Duration) error {
	dc := NewDaemonClient()
	expectedPeers := len(c.Nodes) - 1

	deadline := time.Now().Add(timeout)
	const interval = 2 * time.Second

	var lastErr error
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := checkClusterHealthy(ctx, c, ssh, dc, expectedPeers); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("e2e harness: cluster did not reach healthy within %s: %w", timeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func checkClusterHealthy(ctx context.Context, c *Cluster, ssh SSH, dc *DaemonClient, expectedPeers int) error {
	for _, n := range c.Nodes {
		if _, err := dc.Health(ctx, n); err != nil {
			return fmt.Errorf("%s daemon health: %w", n.Name, err)
		}
		peers, err := natsPeerCount(ctx, ssh, n)
		if err != nil {
			return fmt.Errorf("%s nats peers: %w", n.Name, err)
		}
		if peers < expectedPeers {
			return fmt.Errorf("%s nats peers: have %d, want %d", n.Name, peers, expectedPeers)
		}
	}
	return nil
}

// natsPeerCount returns the number of unique peer names from the node-local
// NATS /routez endpoint (127.0.0.1:8222).
func natsPeerCount(ctx context.Context, ssh SSH, n Node) (int, error) {
	out, err := ssh.Run(ctx, n, "curl -fsS http://127.0.0.1:8222/routez")
	if err != nil {
		return 0, fmt.Errorf("curl routez: %w", err)
	}
	var rz routezResponse
	if err := json.Unmarshal(out, &rz); err != nil {
		return 0, fmt.Errorf("parse routez: %w", err)
	}
	seen := make(map[string]struct{})
	for _, r := range rz.Routes {
		if r.RemoteName == "" {
			continue
		}
		seen[r.RemoteName] = struct{}{}
	}
	return len(seen), nil
}

// AssertCleanState verifies no node has leftover DROP rules or netem qdiscs.
// A dirty baseline is logged and reset via ResetAllNodes; only a reset failure
// is fatal.
func AssertCleanState(ctx context.Context, t *testing.T, c *Cluster, ssh SSH) {
	t.Helper()

	var dirty []string
	for _, n := range c.Nodes {
		drops, err := countDropRules(ctx, ssh, n)
		if err != nil {
			t.Fatalf("e2e harness: inspect iptables on %s: %v", n.Name, err)
		}
		ifaces, err := netemIfaces(ctx, ssh, n)
		if err != nil {
			t.Fatalf("e2e harness: inspect netem on %s: %v", n.Name, err)
		}
		if drops > 0 {
			dirty = append(dirty, fmt.Sprintf("%s: %d iptables DROP rules", n.Name, drops))
		}
		if len(ifaces) > 0 {
			dirty = append(dirty, fmt.Sprintf("%s: netem on %v", n.Name, ifaces))
		}
	}

	if len(dirty) == 0 {
		return
	}
	t.Logf("e2e harness: dirty baseline before scenario — %s; running ResetAllNodes", strings.Join(dirty, "; "))
	if err := ResetAllNodes(ctx, c, ssh); err != nil {
		t.Fatalf("e2e harness: reset after dirty baseline: %v", err)
	}
}

// countDropRules counts DROP rules in INPUT/OUTPUT installed by PartitionNode.
// `|| true` handles grep's no-match exit 1; `2>/dev/null` suppresses sudo noise.
func countDropRules(ctx context.Context, ssh SSH, n Node) (int, error) {
	const cmd = `sudo iptables -S 2>/dev/null | grep -E '^-A (INPUT|OUTPUT) ' | grep -c -- '-j DROP' || true`
	out, err := ssh.Run(ctx, n, cmd)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return 0, nil
	}
	count, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("parse iptables drop count %q (raw output: %q): %w", s, string(out), err)
	}
	return count, nil
}
