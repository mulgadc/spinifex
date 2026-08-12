package host

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// firewallApplyHelper is the fixed-verb root helper installed by setup.sh. The
// daemon's unit sets ProtectSystem=full, so it cannot write /etc itself, and a
// NOPASSWD grant for nft would be root-equivalent.
var firewallApplyHelper = "/usr/local/lib/spinifex/spinifex-firewall-apply"

// firewallPeersPath is read (not written) here to skip a no-op reconcile.
var firewallPeersPath = "/etc/spinifex/firewall/peers.nft"

// ovnEncapCommand is a var so tests can stand in for the Southbound query.
// Unprivileged: the remote is TCP, so this needs no local socket and no sudo
// grant. setup.sh deliberately grants ovn-sbctl to nobody, because its
// arguments are unrestricted enough to be root-equivalent.
// natsRoutePattern pulls the address out of a nats-route:// URL, whose userinfo
// carries the cluster token and must not be mistaken for the host.
var natsRoutePattern = regexp.MustCompile(`nats-route://(?:[^@/]*@)?([0-9.]+):\d+`)

var ovnEncapCommand = func(remote string) *exec.Cmd {
	return exec.Command("ovn-sbctl", "--db="+remote, "--timeout=10",
		"--bare", "--columns=ip", "list", "Encap")
}

// ReconcileFirewall keeps the host firewall's peer sets in step with cluster
// membership. Runs on every startup, so a node added or replaced after
// bootstrap is picked up without re-running an installer. This is the only
// place that enables the policy: the four formation paths (install-node.sh,
// ansible, bm-bootstrap.sh, ISO/single-node) all reach it, and none of them
// would otherwise.
func ReconcileFirewall(configPath string, clusterConfig *config.ClusterConfig) error {
	// A nil config means membership is unknown. Leaving the existing policy in
	// place beats replacing it with a set that would lock this node out.
	if clusterConfig == nil {
		return nil
	}

	// Off means off on a node that was previously on, not merely "skip": leaving
	// a stale drop policy loaded after an operator disables it would be the kind
	// of surprise the toggle exists to avoid.
	if !clusterConfig.Network.FirewallEnabled {
		return disableFirewall()
	}

	// Encap addresses are not in the cluster config, and guessing them from the
	// lan plane would drop Geneve on any node whose vpc bridge is genuinely
	// separate. The Southbound Encap table is authoritative, so when it cannot
	// be read the reconcile is skipped rather than narrowed onto a guess.
	encap, err := ovnEncapAddrs(clusterConfig)
	if err != nil {
		return fmt.Errorf("read OVN encap addresses: %w", err)
	}
	if len(encap) == 0 {
		return fmt.Errorf("OVN reports no encap addresses; refusing to write a peer set that would drop Geneve")
	}

	peers := clusterPeerAddrs(configPath, clusterConfig, encap)
	if len(peers) == 0 {
		return nil
	}

	desired := renderPeersFile(peers, encap)
	if current, err := os.ReadFile(firewallPeersPath); err == nil && string(current) == desired {
		return nil
	}

	cmd := utils.SudoCommand(firewallApplyHelper, "set-peers")
	cmd.Stdin = strings.NewReader(strings.Join(peers, ",") + "\n" + strings.Join(encap, ",") + "\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("set firewall peers: %w: %s", err, strings.TrimSpace(string(out)))
	}

	slog.Info("firewall: peer sets reconciled", "peers", len(peers), "encap_peers", len(encap))
	return nil
}

// disableFirewall removes only spinifex's own table. A no-op when it was never
// loaded, so this is safe to run on every startup of a node that has the
// firewall switched off.
func disableFirewall() error {
	if _, statErr := os.Stat(firewallPeersPath); errors.Is(statErr, os.ErrNotExist) {
		return nil
	}
	out, err := utils.SudoCommand(firewallApplyHelper, "disable").CombinedOutput()
	if err != nil {
		return fmt.Errorf("disable firewall: %w: %s", err, strings.TrimSpace(string(out)))
	}
	slog.Info("firewall: disabled by config, policy removed")
	return nil
}

// clusterPeerAddrs is every address a cluster member might send from, across
// all three planes. No single source has them all: a node's own config names
// its peers only by their wan address, so the lan plane comes from the NATS
// cluster routes and the vpc plane from the encap list already read from OVN.
// A missing address here silently drops working cluster traffic, so this errs
// wide rather than narrow.
func clusterPeerAddrs(configPath string, clusterConfig *config.ClusterConfig, encap []string) []string {
	seen := make(map[string]struct{})
	add := func(addr string) {
		if addr = strings.TrimSpace(addr); isPlainIPv4(addr) {
			seen[addr] = struct{}{}
		}
	}

	for _, node := range clusterConfig.Nodes {
		add(node.Host)
		add(node.AdvertiseIP)
		// The OVN remotes name the database nodes on the lan plane, which is
		// where 6641-6644 are dialled from.
		for remote := range strings.SplitSeq(node.VPCD.OVNNBAddr+","+node.VPCD.OVNSBAddr, ",") {
			add(hostFromRemote(remote))
		}
	}

	for _, addr := range natsRouteAddrs(configPath) {
		add(addr)
	}
	for _, addr := range encap {
		add(addr)
	}

	return sortedKeys(seen)
}

// natsRouteAddrs reads the peer addresses out of the generated NATS config.
// Formation writes every member's cluster-plane address there, on every node,
// which is the only complete record of the lan plane a node holds locally.
// Absent on a single-node cluster, where there are no routes to list.
func natsRouteAddrs(configPath string) []string {
	if configPath == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(configPath), "nats", "nats.conf"))
	if err != nil {
		return nil
	}

	var out []string
	for _, match := range natsRoutePattern.FindAllStringSubmatch(string(data), -1) {
		out = append(out, match[1])
	}
	return out
}

// hostFromRemote takes the address out of an OVSDB remote such as
// "tcp:10.9.7.2:6642", ignoring anything that is not host:port shaped.
func hostFromRemote(remote string) string {
	parts := strings.Split(strings.TrimSpace(remote), ":")
	if len(parts) != 3 {
		return ""
	}
	return parts[1]
}

// ovnEncapAddrs lists every chassis's tunnel endpoint from the Southbound DB.
// Queried over the configured remote rather than a local socket, because only
// the database nodes have one.
func ovnEncapAddrs(clusterConfig *config.ClusterConfig) ([]string, error) {
	remote := ovnSBRemote(clusterConfig)
	if remote == "" {
		return nil, fmt.Errorf("no OVN Southbound address in config")
	}

	out, err := ovnEncapCommand(remote).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}

	seen := make(map[string]struct{})
	for line := range strings.SplitSeq(string(out), "\n") {
		addr := strings.Trim(strings.TrimSpace(line), `"`)
		if isPlainIPv4(addr) {
			seen[addr] = struct{}{}
		}
	}
	return sortedKeys(seen), nil
}

// ovnSBRemote takes the Southbound address off any node's vpcd section. Every
// node is given the same remote list, so the first one populated will do.
func ovnSBRemote(clusterConfig *config.ClusterConfig) string {
	for _, node := range clusterConfig.Nodes {
		if addr := strings.TrimSpace(node.VPCD.OVNSBAddr); addr != "" {
			return addr
		}
	}
	return ""
}

func renderPeersFile(peers, encap []string) string {
	return "# Managed by spinifex-daemon. Regenerated from cluster membership.\n" +
		"define spinifex_peers = { " + strings.Join(peers, ", ") + " }\n" +
		"define spinifex_encap_peers = { " + strings.Join(encap, ", ") + " }\n"
}

// isPlainIPv4 rejects anything that is not a bare dotted quad, including the
// host:port and wildcard forms that appear elsewhere in the config. The helper
// validates again as root; this keeps obvious junk out of the request.
func isPlainIPv4(addr string) bool {
	// netip.Is4 is false for the 4-in-6 form, which nft will not take in an
	// ipv4_addr set. net.IP.To4 would accept it. Loopback is dropped because a
	// single-node cluster names itself that way in the OVN remotes, and `iif lo
	// accept` already covers it — in a source set it could never match.
	ip, err := netip.ParseAddr(addr)
	return err == nil && ip.Is4() && !ip.IsUnspecified() && !ip.IsLoopback()
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
