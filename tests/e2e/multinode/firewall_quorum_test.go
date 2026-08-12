//go:build e2e

package multinode

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

const (
	firewallPeersFile  = "/etc/spinifex/firewall/peers.nft"
	firewallConfigFile = "/etc/spinifex/spinifex.toml"
	firewallConfigBak  = "/var/tmp/spinifex.toml.e2e-firewall-quorum"

	// The phantom entry only has to raise the expected node count. It must carry
	// no OVN remote: the Southbound address is taken from the first node that has
	// one and map order is random, so a bogus remote would fail the query and
	// stop the reconcile at the unreachable path instead of the quorum gate.
	firewallPhantomNode = `printf '\n[nodes.e2ephantom]\nhost = "10.99.99.99"\n'`

	firewallQuorumLog = "chassis encap addresses; not writing a partial peer set"
)

// runFirewallChassisQuorum proves the daemon refuses to write a peer set built
// from a partial chassis list. That list is the normal state early in a
// bootstrap, and writing it produces a firewall that drops Geneve from every
// node missing from it — a policy that looks applied and silently breaks the
// overlay.
//
// The count is inflated rather than OVN broken. Deleting a chassis would
// reproduce the race more literally at the cost of disrupting a node's overlay
// mid-suite; adding a node the cluster does not have reaches the same branch
// with nothing in OVN disturbed.
func runFirewallChassisQuorum(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Multinode — Firewall Chassis Quorum")

	// Last node, not the first: this restarts the node's daemon, and the trio and
	// the operator gateway both lean on the first.
	node := fix.Cluster.Nodes[len(fix.Cluster.Nodes)-1]

	baseline := string(harness.PeerFileContents(t, node, firewallPeersFile))
	require.Containsf(t, baseline, "spinifex_encap_peers",
		"%s has no encap peer set to start from, so there is nothing to protect", node.Name)

	harness.Step(t, "add a phantom node to %s, raising the expected chassis count by one", node.Name)
	firewallRun(t, node, "sudo cp -a "+firewallConfigFile+" "+firewallConfigBak)

	// Safety net only — the body restores and asserts recovery, then drops the
	// backup, so this fires just when the test fails partway.
	t.Cleanup(func() {
		if _, err := firewallRunErr(node, "test -e "+firewallConfigBak); err != nil {
			return
		}
		firewallRestore(t, node)
	})

	firewallRun(t, node, firewallPhantomNode+" | sudo tee -a "+firewallConfigFile+" >/dev/null")

	since := strings.TrimSpace(firewallRun(t, node, `date -u '+%Y-%m-%d %H:%M:%S'`))
	harness.Step(t, "drop the peer file and restart spinifex-daemon on %s", node.Name)
	firewallRun(t, node, "sudo rm -f "+firewallPeersFile+" && sudo systemctl restart spinifex-daemon")

	// Backoff starts at 15s, so the first attempt lands well inside this window.
	journal := "sudo journalctl -u spinifex-daemon --since '" + since + "' --no-pager"
	require.Eventuallyf(t, func() bool {
		out, err := firewallRunErr(node, journal)
		return err == nil && strings.Contains(out, firewallQuorumLog)
	}, 90*time.Second, 3*time.Second,
		"%s never logged the chassis quorum gate; it either wrote a partial peer set or failed somewhere earlier", node.Name)

	harness.Step(t, "assert no peer file was written while the chassis list is short")
	present := strings.TrimSpace(firewallRun(t,
		node, "sudo test -e "+firewallPeersFile+" && echo present || echo absent"))
	require.Equalf(t, "absent", present,
		"%s wrote %s from a partial chassis list", node.Name, firewallPeersFile)

	// The loaded ruleset is runtime state and outlives the peer file, which is
	// what keeps the node protected while the reconcile is refusing to write.
	_, err := firewallRunErr(node, "sudo nft list table inet spinifex_filter")
	require.NoErrorf(t, err, "%s lost its firewall table while the reconcile was blocked", node.Name)

	harness.Step(t, "restore the config and confirm the reconcile recovers on its own")
	firewallRestore(t, node)
	harness.WaitNodeServiceReady(t, node, harness.WithTimeout(90*time.Second))

	var restored string
	require.Eventuallyf(t, func() bool {
		out, err := firewallRunErr(node, "sudo cat "+firewallPeersFile)
		restored = out
		return err == nil && strings.Contains(out, "spinifex_encap_peers")
	}, 90*time.Second, 3*time.Second, "%s never rewrote %s after the phantom was removed", node.Name, firewallPeersFile)

	require.Equalf(t, baseline, restored,
		"%s recovered to a different peer set than it started with", node.Name)
	harness.Detail(t, "peer_set_restored", "byte-identical")

	firewallRun(t, node, "sudo rm -f "+firewallConfigBak)
}

// firewallRestore puts the saved config back and restarts the daemon so the
// next reconcile sees the real node count.
func firewallRestore(t *testing.T, node harness.Node) {
	t.Helper()
	firewallRun(t, node, "sudo cp -a "+firewallConfigBak+" "+firewallConfigFile+
		" && sudo systemctl restart spinifex-daemon")
}

func firewallRun(t *testing.T, node harness.Node, cmd string) string {
	t.Helper()
	out, err := firewallRunErr(node, cmd)
	require.NoErrorf(t, err, "%s on %s: %s", cmd, node.Name, out)
	return out
}

func firewallRunErr(node harness.Node, cmd string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := harness.NewPeerSSH().Run(ctx, node.Addr, cmd)
	return string(out), err
}
