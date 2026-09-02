package host

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// SeedNexthopMAC installs a static OVN MAC binding on the gateway LRP (lrpName)
// for the router's upstream default-route nexthop, so egress does not depend on
// lazy dynamic ARP — which can be lost during gateway bring-up, stranding SNAT'd
// egress in lr_in_arp_resolve (100% loss). For a remote nexthop the MAC is the
// host's own default-gateway MAC (the host shares the physical uplink), read from
// the kernel neigh table and primed with one ping when absent; for a host-local
// nexthop (routed NAT) it is the MAC of the link carrying that address.
// Idempotent: an existing binding for the same lrpName+ip is replaced. A remote
// nexthop that stays unresolved is best-effort — it logs and returns nil, leaving
// dynamic ARP as the fallback. A host-local one returns an error instead, because
// nothing there will ever answer an ARP and the egress loss is permanent. nbAddr
// selects the NB DB to write to (empty uses the local default socket); a compute
// node runs no database, so a write without it would never reach the cluster.
func SeedNexthopMAC(ctx context.Context, runner Runner, nbAddr, lrpName, nexthopIP string) error {
	if lrpName == "" || nexthopIP == "" {
		return nil
	}
	if runner == nil {
		runner = NewExecRunner()
	}

	dev, err := nexthopDev(ctx, runner, nexthopIP)
	if err != nil {
		return fmt.Errorf("resolve egress dev for nexthop %s: %w", nexthopIP, err)
	}

	var mac string
	if dev == localRouteDev {
		// Routed NAT: the nexthop is an address on this host, so no neigh entry
		// can ever exist and pinging it would prime nothing.
		mac, err = localNexthopMAC(ctx, runner, nexthopIP)
		if err != nil {
			// A host-local nexthop has no dynamic-ARP fallback, so an unresolved
			// binding black-holes egress permanently rather than degrading it.
			return fmt.Errorf("resolve host-local nexthop %s for %s: %w; egress will black-hole — check %s is up carrying %s",
				nexthopIP, lrpName, err, NATTransitHostEnd, NATTransitGatewayCIDR)
		}
	} else {
		mac = nexthopMAC(ctx, runner, nexthopIP, dev)
		if mac == "" {
			// Prime the kernel neigh table once, then re-read.
			_, _ = runner.Run(ctx, "ping", "-c", "1", "-W", "1", nexthopIP)
			mac = nexthopMAC(ctx, runner, nexthopIP, dev)
		}
		if mac == "" {
			slog.Warn("host: nexthop MAC unresolved; leaving dynamic ARP fallback",
				"lrp", lrpName, "nexthop", nexthopIP, "dev", dev)
			return nil
		}
	}

	var db []string
	if nbAddr != "" {
		db = []string{"--db=" + nbAddr, "--no-leader-only"}
	}

	// Idempotent: drop any stale binding (best-effort) before adding.
	_, _ = runner.Run(ctx, "ovn-nbctl", append(append([]string{}, db...), "--if-exists", "static-mac-binding-del", lrpName, nexthopIP)...)
	if _, err := runner.Run(ctx, "ovn-nbctl", append(append([]string{}, db...), "static-mac-binding-add", lrpName, nexthopIP, mac)...); err != nil {
		return fmt.Errorf("ovn-nbctl static-mac-binding-add %s %s %s: %w", lrpName, nexthopIP, mac, err)
	}

	slog.Info("host: seeded static OVN MAC binding for gateway nexthop",
		"lrp", lrpName, "nexthop", nexthopIP, "mac", mac)
	return nil
}

// localRouteDev is the dev `ip route get` reports for an address owned by this
// host; the address itself still lives on a real link.
const localRouteDev = "lo"

// localNexthopMAC returns the MAC of the link carrying ip, for a nexthop the
// kernel routes to lo. In routed NAT that is the host end of the transit veth,
// which is what OVN must address to reach the nexthop over the uplink bridge.
// Every failure is distinguishable: `ip addr show to` exits 0 with no output
// when nothing matches, so a non-nil error there is never the no-match case.
func localNexthopMAC(ctx context.Context, runner Runner, ip string) (string, error) {
	out, err := runner.Run(ctx, "ip", "-4", "-o", "addr", "show", "to", ip+"/32")
	if err != nil {
		return "", fmt.Errorf("ip -4 -o addr show to %s/32: %w", ip, err)
	}
	dev := parseAddrShowDev(string(out))
	if dev == "" {
		return "", fmt.Errorf("no link carries %s", ip)
	}
	link, err := runner.Run(ctx, "ip", "-o", "link", "show", "dev", dev)
	if err != nil {
		return "", fmt.Errorf("ip -o link show dev %s: %w", dev, err)
	}
	mac := parseLinkEtherMAC(string(link))
	if mac == "" {
		return "", fmt.Errorf("link %s carrying %s has no ethernet address", dev, ip)
	}
	return mac, nil
}

// parseAddrShowDev returns the first non-loopback link name from `ip -o addr
// show` output, e.g. "5: spx-nat-host    inet 100.127.0.1/24 scope global ...".
func parseAddrShowDev(out string) string {
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[1] == localRouteDev {
			continue
		}
		return fields[1]
	}
	return ""
}

// parseLinkEtherMAC extracts the link/ether address from `ip -o link show`
// output. Returns "" for a link with no ethernet address (loopback, tun).
func parseLinkEtherMAC(out string) string {
	fields := strings.Fields(out)
	for i, f := range fields {
		if f == "link/ether" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

// nexthopDev resolves the egress interface the kernel uses to reach ip, via
// `ip route get`. Mirrors routeDev in gateway_claim.go but through Runner.
func nexthopDev(ctx context.Context, runner Runner, ip string) (string, error) {
	out, err := runner.Run(ctx, "ip", "route", "get", ip)
	if err != nil {
		return "", fmt.Errorf("ip route get %s: %w", ip, err)
	}
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "dev" && i+1 < len(fields) {
			return fields[i+1], nil
		}
	}
	return "", fmt.Errorf("ip route get %s: no dev in %q", ip, strings.TrimSpace(string(out)))
}

// nexthopMAC reads the kernel neigh table for ip on dev and extracts a usable
// lladdr MAC. Returns "" on a missing/unresolved entry.
func nexthopMAC(ctx context.Context, runner Runner, ip, dev string) string {
	out, err := runner.Run(ctx, "ip", "neigh", "show", ip, "dev", dev)
	if err != nil {
		return ""
	}
	return parseNeighMAC(string(out))
}

// parseNeighMAC extracts the lladdr token from an `ip neigh show` line, e.g.
// "192.168.1.1 dev br-wan lladdr 04:f4:1c:fd:56:27 REACHABLE". Returns "" for
// an unresolved (FAILED/INCOMPLETE) or empty entry.
func parseNeighMAC(out string) string {
	out = strings.TrimSpace(out)
	if out == "" || !strings.Contains(out, "lladdr") {
		return ""
	}
	if strings.Contains(out, "FAILED") || strings.Contains(out, "INCOMPLETE") {
		return ""
	}
	fields := strings.Fields(out)
	for i, f := range fields {
		if f == "lladdr" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}
