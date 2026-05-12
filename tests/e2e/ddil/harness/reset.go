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

// routezResponse is the subset of the NATS /routez monitoring payload the
// harness consumes. Only remote_name is required because the harness counts
// unique peer names (not raw route count — NATS opens multiple TCP routes
// per peer and num_routes would overcount). The shell suite's
// verify_nats_cluster helper uses the same predicate.
type routezResponse struct {
	Routes []struct {
		RemoteName string `json:"remote_name"`
	} `json:"routes"`
}

// ResetAllNodes returns the cluster to a known clean state: every node has
// all iptables rules flushed, every tc netem qdisc removed, and both
// spinifex-nats and spinifex-daemon running. After the per-node reset, it
// waits for cluster health (every daemon /health returns 200 and each node's
// NATS monitor reports len(Nodes)-1 unique route peers) before returning.
//
// Idempotent — safe against a clean cluster. Three callers:
//  1. harness.Run, between a failed scenario and its retry.
//  2. AssertCleanState, when a pre-scenario baseline is dirty.
//  3. The suite-level signal handler in TestMain, so SIGINT/SIGTERM do not
//     leave the cluster partitioned or netem-shaped.
func ResetAllNodes(ctx context.Context, c *Cluster, ssh SSH) error {
	for _, n := range c.Nodes {
		if err := resetNode(ctx, ssh, n); err != nil {
			return err
		}
	}
	return waitClusterHealthy(ctx, c, ssh, 2*time.Minute)
}

func resetNode(ctx context.Context, ssh SSH, n Node) error {
	// Flush every DDIL-owned firewall rule. iptables -F on the default
	// (filter) table covers INPUT/OUTPUT DROPs installed by PartitionNode;
	// iptables -X removes any transient user chains a scenario happens to
	// leave behind. This matches HealNode's teardown.
	if _, err := ssh.Run(ctx, n, "sudo iptables -F && sudo iptables -X"); err != nil {
		return fmt.Errorf("ddil harness: reset iptables on %s: %w", n.Name, err)
	}

	// Remove netem qdiscs only on interfaces that actually carry one.
	// Scenarios may pick different NICs (eth0, ens18, br-mgmt) and a fixed
	// interface list here would silently miss whichever one was used.
	ifaces, err := netemIfaces(ctx, ssh, n)
	if err != nil {
		return err
	}
	for _, iface := range ifaces {
		cmd := fmt.Sprintf("sudo tc qdisc del dev %s root 2>/dev/null || true", shellQuote(iface))
		if _, err := ssh.Run(ctx, n, cmd); err != nil {
			return fmt.Errorf("ddil harness: clear netem on %s (%s): %w", n.Name, iface, err)
		}
	}

	// Start services — noop if already active (systemd returns 0).
	if _, err := ssh.Run(ctx, n, "sudo systemctl start spinifex-nats spinifex-daemon"); err != nil {
		return fmt.Errorf("ddil harness: start services on %s: %w", n.Name, err)
	}
	return nil
}

// netemIfaces lists every interface on node whose root qdisc is a netem
// qdisc. Used by resetNode to decide what to tear down and by
// AssertCleanState to detect a dirty baseline.
func netemIfaces(ctx context.Context, ssh SSH, n Node) ([]string, error) {
	// `tc qdisc show` prints one row per qdisc; `qdisc netem 8003: dev eth0
	// root ...` → awk $5 yields the interface name.
	out, err := ssh.Run(ctx, n, "tc qdisc show | awk '/qdisc netem/ {print $5}' | sort -u")
	if err != nil {
		return nil, fmt.Errorf("ddil harness: list netem qdiscs on %s: %w", n.Name, err)
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
			return fmt.Errorf("ddil harness: cluster did not reach healthy within %s: %w", timeout, lastErr)
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

// natsPeerCount SSHes into node, curls the node-local NATS monitor endpoint
// (bound to 127.0.0.1:8222 by the spinifex NATS config), and returns the
// number of unique peer names it reports.
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

// AssertCleanState verifies no node carries a DDIL-owned firewall rule or
// netem qdisc before a scenario begins. A dirty baseline is not itself a
// test failure — it usually means a previous scenario's t.Cleanup did not
// run (panic, SIGKILL, CI runner killed) — so the helper logs details and
// calls ResetAllNodes to recover. Only a failure of that reset is fatal.
func AssertCleanState(ctx context.Context, t *testing.T, c *Cluster, ssh SSH) {
	t.Helper()

	var dirty []string
	for _, n := range c.Nodes {
		drops, err := countDropRules(ctx, ssh, n)
		if err != nil {
			t.Fatalf("ddil harness: inspect iptables on %s: %v", n.Name, err)
		}
		ifaces, err := netemIfaces(ctx, ssh, n)
		if err != nil {
			t.Fatalf("ddil harness: inspect netem on %s: %v", n.Name, err)
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
	t.Logf("ddil harness: dirty baseline before scenario — %s; running ResetAllNodes", strings.Join(dirty, "; "))
	if err := ResetAllNodes(ctx, c, ssh); err != nil {
		t.Fatalf("ddil harness: reset after dirty baseline: %v", err)
	}
}

// countDropRules reports how many DROP rules are installed in INPUT/OUTPUT
// on node. `iptables -S` without a chain prints every chain in
// iptables-save format (`-A INPUT ... -j DROP`); filtering for the two
// chains we care about keeps the count scoped to PartitionNode's targets.
// `2>/dev/null` swallows sudo/iptables stderr so a noisy environment does
// not poison the grep input, and `|| true` covers grep's no-match exit 1.
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
