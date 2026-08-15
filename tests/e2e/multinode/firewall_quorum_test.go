//go:build e2e

package multinode

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	firewallPeersFile  = "/etc/spinifex/firewall/peers.nft"
	firewallModeFile   = "/etc/spinifex/firewall/mode"
	firewallConfigFile = "/etc/spinifex/spinifex.toml"
	firewallConfigBak  = "/var/tmp/spinifex.toml.e2e-firewall-quorum"

	// The phantom entry only has to raise the expected node count. It must carry
	// no OVN remote: the Southbound address is taken from the first node that has
	// one and map order is random, so a bogus remote would fail the query and
	// stop the reconcile at the unreachable path instead of the quorum gate.
	firewallPhantomNode = `printf '\n[nodes.e2ephantom]\nhost = "10.99.99.99"\n'`

	// The reconcile reports the short list as "OVN reports N of M chassis encap
	// addresses", and MaintainFirewall carries it into the retry warning. There
	// is no separate message for the gate itself.
	firewallQuorumLog = "chassis encap addresses"
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

	firewallArm(t, node)

	// A wait, not a read: with the policy on, the daemon writes the peer set once
	// every chassis has registered, and it retries on a backoff. install-node.sh
	// allows the same 180s for the same thing.
	harness.Step(t, "wait for %s to write a peer set", node.Name)
	var baseline string
	if !assert.Eventually(t, func() bool {
		out, err := firewallRunErr(node, "sudo cat "+firewallPeersFile)
		baseline = out
		return err == nil && strings.Contains(out, "spinifex_encap_peers")
	}, 180*time.Second, 5*time.Second) {
		t.Fatalf("%s never wrote an encap peer set to %s, so there is nothing to protect\n%s",
			node.Name, firewallPeersFile, firewallWhyNoPeers(node))
	}

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
	var tail string
	if !assert.Eventually(t, func() bool {
		out, err := firewallRunErr(node, journal)
		tail = out
		return err == nil && strings.Contains(out, firewallQuorumLog)
	}, 90*time.Second, 3*time.Second) {
		t.Fatalf("%s never logged the chassis quorum gate; it either wrote a partial peer set or failed somewhere earlier\n%s%s",
			node.Name, firewallWhyNoPeers(node), firewallTail(tail, 25))
	}

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

// firewallArm switches the host firewall on for the duration of the test, and
// back to whatever the installer chose afterwards. Only the ISO arms by
// default, so on every other install path the policy under test is not loaded.
func firewallArm(t *testing.T, node harness.Node) {
	t.Helper()

	// Absent means on, the same way the daemon reads it: a node installed before
	// the mode file existed keeps the policy it already has.
	mode := strings.TrimSpace(firewallRun(t, node,
		"sudo cat "+firewallModeFile+" 2>/dev/null || echo on"))
	if mode == "on" {
		return
	}

	harness.Step(t, "arm the host firewall on %s, which was installed %q", node.Name, mode)

	// Registered before the policy is switched on, so it runs after the config
	// restore below it and leaves the node the way the installer left it. The
	// daemon removes the loaded ruleset itself once the mode says off again.
	t.Cleanup(func() {
		_, _ = firewallRunErr(node, "printf '%s\\n' "+harness.ShellQuote(mode)+
			" | sudo tee "+firewallModeFile+" >/dev/null && sudo systemctl restart spinifex-daemon")
	})

	firewallRun(t, node, "printf 'on\\n' | sudo tee "+firewallModeFile+
		" >/dev/null && sudo systemctl restart spinifex-daemon")
}

// firewallWhyNoPeers reports what decides whether the daemon can write a peer
// set. The daemon's own journal is the authority on the chassis count: asking
// ovn-sbctl here would query a follower and report an empty cluster.
func firewallWhyNoPeers(node harness.Node) string {
	var b strings.Builder
	for _, probe := range []struct{ label, cmd string }{
		{"firewall mode", "sudo cat " + firewallModeFile + " 2>/dev/null || echo '(absent, means on)'"},
		{"nodes named in the config", "sudo grep -c '^\\[nodes\\.' " + firewallConfigFile},
		{"daemon on firewall", "sudo journalctl -u spinifex-daemon --no-pager -n 400 | grep -i firewall | tail -10"},
	} {
		out, err := firewallRunErr(node, probe.cmd)
		if err != nil {
			out = "(" + err.Error() + ")"
		}
		fmt.Fprintf(&b, "         %s: %s\n", probe.label, strings.TrimSpace(out))
	}
	return b.String()
}

// firewallTail keeps a failure message readable when the journal window is wide.
func firewallTail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
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
