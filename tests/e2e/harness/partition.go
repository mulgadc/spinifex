//go:build e2e

package harness

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// partitionTable is the nft table a partition lives in, and it is its own table
// on purpose.
//
// The node's real firewall is inet spinifex_filter, built by setup.sh, and a
// test that flushed or edited it would leave the node either unfirewalled or
// misfirewalled with no sign of which. nftables evaluates every table at each
// hook and a drop in any of them wins, so a separate table expresses exactly
// "these packets, dropped, and nothing else" — and removing it is one atomic
// delete that cannot take a rule the node needed with it.
const partitionTable = "spx_e2e_partition"

// partitionHealUnit is a transient systemd timer that removes the table on its
// own, so a run killed between the partition and the heal — a cancelled
// workflow, a panicking test, a severed orchestrator — does not leave a node
// cut off from its cluster until somebody notices.
const partitionHealUnit = "spx-e2e-partition-heal"

// partitionDeadman is how long that timer waits. Longer than any scenario that
// installs a partition, short enough that an abandoned one is gone before the
// next nightly reaches this cell.
const partitionDeadman = 30 * time.Minute

// PortSet is a named group of TCP ports a partition cuts. Named because the
// name is what the test output says was severed, and "nats" is a fault a reader
// can reason about where a list of numbers is not.
type PortSet struct {
	Name  string
	Ports []int
}

// The port sets worth cutting on their own. Each is a different failure, and
// they are deliberately not combined into one "isolate the node": a node that
// has lost consensus but can still read its volumes behaves differently from one
// that has lost both, and a test that could not tell them apart would be
// asserting whichever happened.
var (
	// NATSPorts is the client port and the cluster route port. Cutting both
	// leaves the node's own NATS server running and its local clients connected
	// while severing the Raft routes, which is the half-healthy state a real
	// network fault produces: services up, consensus unreachable.
	NATSPorts = PortSet{Name: "nats", Ports: []int{4222, 4248}}

	// ObjectStorePorts is predastore's gate and its admin surface. A node that
	// cannot reach these cannot read its guests' volumes, which is a storage
	// fault rather than a partition and is answered differently.
	ObjectStorePorts = PortSet{Name: "predastore", Ports: []int{8443, 8660}}
)

// PartitionPeerPorts drops TCP traffic on the given ports between target and
// every other node in the cluster, in both directions, and returns the heal.
//
// Both directions and both sport and dport, because a node both dials its peers
// and accepts their connections, and a partition that cut only one of those
// would leave half the conversation working — which is a fault, but not the one
// being asked about, and not one any production network produces.
//
// SSH is proved still working afterwards. If the orchestrator reaches these
// nodes on an address that is also a peer address, the partition would sever the
// test's own control path, and failing there names that rather than timing out
// later with every assertion unexplained.
func PartitionPeerPorts(t *testing.T, cluster *Cluster, target Node, sets ...PortSet) (heal func()) {
	t.Helper()
	if len(sets) == 0 {
		t.Fatalf("PartitionPeerPorts %s: no port sets given, so nothing would be severed", target.Name)
	}

	peers := cluster.Peers(target)
	if len(peers) == 0 {
		t.Fatalf("PartitionPeerPorts %s: no peers in the cluster, so there is nothing to partition from", target.Name)
	}

	var names, ports []string
	for _, set := range sets {
		names = append(names, set.Name)
		for _, port := range set.Ports {
			ports = append(ports, strconv.Itoa(port))
		}
	}
	var addrs []string
	for _, peer := range peers {
		addrs = append(addrs, ShellQuote(peer.Addr))
	}

	Step(t, "partition %s from %v on %s (ports %s)",
		target.Name, nodeNames(peers), strings.Join(names, "+"), strings.Join(ports, ","))

	// Resolved on the node, not here: it is the node's own resolution that
	// decides where its packets go, and the orchestrator may not share it.
	script := fmt.Sprintf(`set -e
peers=
for host in %s; do
    addr=$(getent ahostsv4 "$host" | awk 'NR==1{print $1}')
    [ -n "$addr" ] || { echo "cannot resolve $host on this node" >&2; exit 1; }
    peers="${peers:+$peers, }$addr"
done
sudo systemctl stop %s.timer 2>/dev/null || true
sudo nft -f - <<NFT
table inet %s
delete table inet %s
table inet %s {
    set peers { type ipv4_addr; elements = { $peers } }
    set ports { type inet_service; elements = { %s } }
    chain input {
        type filter hook input priority -300; policy accept;
        ip saddr @peers tcp dport @ports drop
        ip saddr @peers tcp sport @ports drop
    }
    chain output {
        type filter hook output priority -300; policy accept;
        ip daddr @peers tcp dport @ports drop
        ip daddr @peers tcp sport @ports drop
    }
}
NFT
sudo systemd-run --unit=%s --collect --on-active=%d \
    /usr/sbin/nft delete table inet %s >/dev/null 2>&1
echo "$peers"`,
		strings.Join(addrs, " "),
		partitionHealUnit,
		partitionTable, partitionTable, partitionTable,
		strings.Join(ports, ", "),
		partitionHealUnit, int(partitionDeadman.Seconds()), partitionTable)

	healed := false
	heal = func() {
		if healed {
			return
		}
		healed = true
		Step(t, "heal the partition on %s", target.Name)
		out, err := partitionRun(target, fmt.Sprintf(
			"sudo systemctl stop %s.timer 2>/dev/null || true; "+
				"sudo nft delete table inet %s 2>/dev/null || true; "+
				"sudo nft list tables | grep -c %s || true",
			partitionHealUnit, partitionTable, partitionTable))
		if err != nil {
			t.Errorf("could not heal the partition on %s, which leaves it cut off from its cluster "+
				"until the deadman timer fires: %v\n%s", target.Name, err, out)
			return
		}
		if remaining := strings.TrimSpace(out); remaining != "0" && remaining != "" {
			t.Errorf("the partition table is still present on %s after the heal: %q", target.Name, remaining)
		}
	}
	t.Cleanup(heal)

	out, err := partitionRun(target, script)
	if err != nil {
		t.Fatalf("could not partition %s: %v\n%s", target.Name, err, out)
	}
	Detail(t, "partitioned_from", strings.TrimSpace(out))

	if _, err := partitionRun(target, "echo ok"); err != nil {
		t.Fatalf("the partition severed the orchestrator's own SSH to %s, so the test cannot observe "+
			"anything it does; the orchestrator is probably reachable on a peer address: %v", target.Name, err)
	}

	// The fault is the premise of everything after it, so it is proved rather
	// than assumed. A rule set that installed cleanly and dropped nothing would
	// otherwise read as the system tolerating a partition.
	requirePeerPortUnreachable(t, target, peers[0], sets[0].Ports[0])
	return heal
}

// requirePeerPortUnreachable proves the partition is actually dropping packets.
func requirePeerPortUnreachable(t *testing.T, target, peer Node, port int) {
	t.Helper()

	out, err := partitionRun(target, fmt.Sprintf(
		"timeout 5 bash -c 'cat </dev/null >/dev/tcp/%s/%d' && echo REACHED || echo BLOCKED",
		ShellQuote(peer.Addr), port))
	if err != nil {
		t.Fatalf("could not test the partition from %s: %v\n%s", target.Name, err, out)
	}
	if !strings.Contains(out, "BLOCKED") {
		t.Fatalf("%s can still reach %s:%d after the partition, so nothing below would be a test of one\n%s",
			target.Name, peer.Name, port, out)
	}
}

func partitionRun(node Node, cmd string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, err := NewPeerSSH().Run(ctx, node.Addr, cmd)
	return string(out), err
}

func nodeNames(nodes []Node) []string {
	out := make([]string, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, node.Name)
	}
	return out
}
