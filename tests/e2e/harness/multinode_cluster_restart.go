//go:build e2e

package harness

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A coordinated cluster shutdown followed by a start is the operation every
// software update performs, and it is the one path that had no e2e coverage:
// StopNode/StartNode simulate a hard outage, which deliberately leaves guests
// running, so nothing exercised a guest that was drained to stopped, had its
// volumes sealed, and then had to relaunch from that sealed state against
// dependencies that were themselves still starting. A guest was lost that way.

// clusterShutdownPhaseTimeout is passed to `spx admin cluster shutdown` as its
// per-phase budget. The default is 120s; DRAIN legitimately takes longer when
// several guests unmount at once, and a drain that times out is reported as
// success, so allowing the time is cheaper than the ambiguity.
const clusterShutdownPhaseTimeout = 10 * time.Minute

// spxProcessDrainTimeout bounds the wait for every spx process to exit after
// the units are stopped. Stopping the target returns once the target is
// inactive, which is not the same as every service having exited.
const spxProcessDrainTimeout = 3 * time.Minute

// ClusterShutdownAndRestart performs the full operator shutdown sequence and
// brings the cluster back: the coordinated phased shutdown, a per-unit stop on
// every node, a confirmed wait for every spx process to exit, then a start of
// spinifex.target everywhere. It returns once NATS has reformed and every
// gateway answers, so a caller can immediately assert on guest state.
//
// The spx-process wait is the step that makes this a real restart rather than a
// systemctl call that returned early. spinifex-shutdown.service is ordered
// After= the storage services, so its drain runs while they are still up and
// their stop jobs queue behind it; `systemctl stop` returns as soon as the
// target is inactive, leaving predastore, viperblock and NATS running.
func ClusterShutdownAndRestart(t *testing.T, c *Cluster, env *Env) {
	t.Helper()

	if len(c.Nodes) < 2 {
		t.Fatalf("cluster restart needs a multi-node cluster, have %d", len(c.Nodes))
	}

	ClusterShutdown(t, c)

	Step(t, "start spinifex.target on all %d nodes", len(c.Nodes))
	for _, n := range c.Nodes {
		StartNode(t, n)
	}

	Step(t, "wait NATS to reform to %d peers", len(c.Nodes))
	c.WaitNATSPeers(t, len(c.Nodes), WithTimeout(3*time.Minute), WithPoll(2*time.Second))

	Step(t, "wait every gateway to answer")
	c.WaitGatewayHealthy(t, WithTimeout(3*time.Minute), WithPoll(2*time.Second))

	if env != nil {
		Step(t, "wait every daemon ready")
		c.WaitDaemonReady(t, env, WithTimeout(3*time.Minute), WithPoll(2*time.Second))
	}
}

// ClusterShutdown runs the coordinated shutdown and leaves the cluster down.
// Split out from ClusterShutdownAndRestart so a caller can assert on the
// stopped state, and so a t.Cleanup can restart unconditionally.
func ClusterShutdown(t *testing.T, c *Cluster) {
	t.Helper()

	ssh := NewPeerSSH()
	leader := c.Nodes[0]

	Step(t, "spx admin cluster shutdown via %s", leader.Name)
	ctx, cancel := context.WithTimeout(context.Background(), clusterShutdownPhaseTimeout+2*time.Minute)
	out, err := ssh.Run(ctx, leader.Addr, fmt.Sprintf(
		"sudo spx admin cluster shutdown --timeout %s --config /etc/spinifex/spinifex.toml",
		clusterShutdownPhaseTimeout))
	cancel()
	Detail(t, "cluster_shutdown", string(out))
	if err != nil {
		// The phases can complete and still lose the SSH channel as NATS goes
		// down under it. The per-unit stop below is the authoritative teardown.
		t.Logf("cluster shutdown on %s reported %v — proceeding to the per-unit stop", leader.Name, err)
	}

	Step(t, "stop spinifex units on all %d nodes", len(c.Nodes))
	for _, n := range c.Nodes {
		StopNode(t, n)
		stopUnitsHoldingSpx(t, n)
	}

	Step(t, "confirm no spx process survives on any node")
	for _, n := range c.Nodes {
		waitNoSpxProcess(t, n)
	}
}

// stopUnitsHoldingSpx stops whatever units are still running the spx binary,
// found from the processes themselves rather than from a list.
//
// Every service is the same binary — `spx service <name> start` — so the set
// that has to stop is "whoever is still running it", and a hard-coded list goes
// stale silently the moment a service is added. StopNode's list omits
// spinifex-northstar and spinifex-qmp-collector, which is harmless for the hard
// outage it simulates and not harmless here.
func stopUnitsHoldingSpx(t *testing.T, node Node) {
	t.Helper()

	ssh := NewPeerSSH()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// The unit name comes from each survivor's cgroup, so this needs no
	// knowledge of which units exist.
	const cmd = `for pid in $(pgrep -x spx || true); do
    unit=$(grep -oE 'spinifex-[a-z0-9-]+\.service' "/proc/$pid/cgroup" 2>/dev/null | head -1)
    [ -n "$unit" ] && sudo systemctl stop "$unit"
done; true`
	if _, err := ssh.Run(ctx, node.Addr, cmd); err != nil {
		t.Logf("stopUnitsHoldingSpx %s: %v (the wait below is the gate)", node.Name, err)
	}
}

// waitNoSpxProcess blocks until `pgrep -x spx` finds nothing on node. A
// surviving process means the stop returned before the service did, and
// anything asserted after that is measuring the old process.
func waitNoSpxProcess(t *testing.T, node Node) {
	t.Helper()

	ssh := NewPeerSSH()
	EventuallyErr(t, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// pgrep exits 1 when nothing matches, which is the outcome wanted, so
		// the count is read from stdout rather than from the exit status.
		out, err := ssh.Run(ctx, node.Addr, "pgrep -x spx | wc -l")
		if err != nil {
			return fmt.Errorf("%s pgrep spx: %w", node.Name, err)
		}
		if n := strings.TrimSpace(string(out)); n != "0" {
			// A survivor is a unit that was never asked to stop, or one that
			// systemd restarted. Ask again rather than only polling.
			stopUnitsHoldingSpx(t, node)
			return fmt.Errorf("%s still has %s spx process(es)", node.Name, n)
		}
		return nil
	}, spxProcessDrainTimeout, 3*time.Second)
}
