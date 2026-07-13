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
// egress in lr_in_arp_resolve (100% loss). The nexthop MAC is the host's own
// default-gateway MAC (the host shares the physical uplink), read from the kernel
// neigh table and primed with one ping when absent. Idempotent: an existing
// binding for the same lrpName+ip is replaced. Best-effort: a missing MAC logs
// and returns nil, leaving dynamic ARP as the fallback.
func SeedNexthopMAC(ctx context.Context, runner Runner, lrpName, nexthopIP string) error {
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

	mac := nexthopMAC(ctx, runner, nexthopIP, dev)
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

	// Idempotent: drop any stale binding (best-effort) before adding.
	_, _ = runner.Run(ctx, "ovn-nbctl", "--if-exists", "static-mac-binding-del", lrpName, nexthopIP)
	if _, err := runner.Run(ctx, "ovn-nbctl", "static-mac-binding-add", lrpName, nexthopIP, mac); err != nil {
		return fmt.Errorf("ovn-nbctl static-mac-binding-add %s %s %s: %w", lrpName, nexthopIP, mac, err)
	}

	slog.Info("host: seeded static OVN MAC binding for gateway nexthop",
		"lrp", lrpName, "nexthop", nexthopIP, "mac", mac)
	return nil
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
