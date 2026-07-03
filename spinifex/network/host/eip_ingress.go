package host

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

const eipIngressComment = "spinifex-eip-ingress"

// DetectUplinkFor resolves which interface (and local source IP) the kernel
// would use to reach gatewayIP by parsing `ip route get`. Re-run per ensure:
// WiFi/cellular uplinks change interface and address across reconnects.
func DetectUplinkFor(ctx context.Context, r Runner, gatewayIP string) (iface, srcIP string, err error) {
	if gatewayIP == "" {
		return "", "", fmt.Errorf("DetectUplinkFor: gatewayIP required")
	}
	out, err := r.Run(ctx, "ip", "route", "get", gatewayIP)
	if err != nil {
		return "", "", fmt.Errorf("route lookup for %s: %s: %w", gatewayIP, string(out), err)
	}
	fields := strings.Fields(strings.SplitN(string(out), "\n", 2)[0])
	for i := 0; i < len(fields)-1; i++ {
		switch fields[i] {
		case "dev":
			iface = fields[i+1]
		case "src":
			srcIP = fields[i+1]
		}
	}
	if iface == "" {
		return "", "", fmt.Errorf("no dev in route to %s: %q", gatewayIP, strings.TrimSpace(string(out)))
	}
	return iface, srcIP, nil
}

// uplinkAddr returns the first global IPv4 on iface, or "" when none parses.
// Best-effort: the src hint on the EIP route is an optimization, not plumbing.
func uplinkAddr(ctx context.Context, r Runner, iface string) string {
	out, err := r.Run(ctx, "ip", "-4", "-o", "addr", "show", "dev", iface, "scope", "global")
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	for i := 0; i < len(fields)-1; i++ {
		if fields[i] == "inet" {
			return strings.SplitN(fields[i+1], "/", 2)[0]
		}
	}
	return ""
}

// resolveUplink picks the LAN-facing interface for EIP proxy-ARP: routed
// toward poolGateway when known (static pools), else the configured hint
// (dhcp pools DORA on the uplink itself). Probing the EIP would be wrong —
// once the /32 route lands, the kernel routes it to the transit veth.
func resolveUplink(ctx context.Context, r Runner, poolGateway, uplinkHint string) (iface, srcIP string, err error) {
	if poolGateway != "" {
		return DetectUplinkFor(ctx, r, poolGateway)
	}
	if uplinkHint != "" {
		return uplinkHint, uplinkAddr(ctx, r, uplinkHint), nil
	}
	return "", "", nil
}

func eipForwardRules(eip string) [][]string {
	return [][]string{
		{"-i", NATTransitHostEnd, "-s", eip + "/32",
			"-m", "comment", "--comment", eipIngressComment, "-j", "ACCEPT"},
		{"-o", NATTransitHostEnd, "-d", eip + "/32",
			"-m", "comment", "--comment", eipIngressComment, "-j", "ACCEPT"},
	}
}

// EnsureEIPIngress makes an EIP reachable from the uplink LAN in routed-NAT
// mode: a /32 host route steers the EIP into OVN via the VPC gateway LRP's
// transit IP, a proxy-ARP neighbor entry answers ARP for the EIP on the
// uplink (L3 only — same MAC, so WiFi-safe), and per-EIP FORWARD accepts
// cover DROP-policy hosts. The route carries `src <uplink-IP>` so host-local
// traffic to the EIP still DNATs (host source would match the exempt set).
// Idempotent; call on every EIP bind and reconcile pass.
func EnsureEIPIngress(ctx context.Context, r Runner, eip, gwLrpIP, poolGateway, uplinkHint string) error {
	if eip == "" {
		return fmt.Errorf("EnsureEIPIngress: eip required")
	}
	if gwLrpIP == "" {
		return fmt.Errorf("EnsureEIPIngress: gwLrpIP required")
	}

	uplink, srcIP, err := resolveUplink(ctx, r, poolGateway, uplinkHint)
	if err != nil {
		return fmt.Errorf("EnsureEIPIngress %s: %w", eip, err)
	}

	routeArgs := []string{"route", "replace", eip + "/32", "via", gwLrpIP, "dev", NATTransitHostEnd}
	if srcIP != "" {
		routeArgs = append(routeArgs, "src", srcIP)
	}
	if out, err := r.Run(ctx, "ip", routeArgs...); err != nil {
		return fmt.Errorf("install EIP route %s via %s: %s: %w", eip, gwLrpIP, string(out), err)
	}

	if uplink != "" {
		if out, err := r.Run(ctx, "ip", "neigh", "replace", "proxy", eip, "dev", uplink); err != nil {
			return fmt.Errorf("install proxy-ARP for %s on %s: %s: %w", eip, uplink, string(out), err)
		}
		// Answer proxied ARP immediately instead of the ~800ms default delay.
		if out, err := r.Run(ctx, "sysctl", "-w", "net.ipv4.neigh."+uplink+".proxy_delay=0"); err != nil {
			slog.Debug("host: proxy_delay tune failed (non-fatal)", "uplink", uplink, "out", string(out), "err", err)
		}
		// Gratuitous ARP so LAN peers refresh stale entries for reused EIPs.
		if out, err := r.Run(ctx, "arping", "-U", "-c", "2", "-I", uplink, eip); err != nil {
			slog.Debug("host: gratuitous ARP failed (non-fatal)", "eip", eip, "uplink", uplink, "out", string(out), "err", err)
		}
	}

	for _, spec := range eipForwardRules(eip) {
		if _, err := r.Run(ctx, "iptables", natRuleArgs("-C", "filter", "FORWARD", spec)...); err == nil {
			continue
		}
		if out, err := r.Run(ctx, "iptables", natRuleArgs("-A", "filter", "FORWARD", spec)...); err != nil {
			return fmt.Errorf("install EIP FORWARD rule for %s: %s: %w", eip, string(out), err)
		}
	}

	slog.Info("EIP ingress installed", "eip", eip, "gw_lrp_ip", gwLrpIP, "uplink", uplink, "src", srcIP)
	return nil
}

// RemoveEIPIngress tears down the host state for an EIP on disassociate or
// release. Missing pieces are not errors (idempotent teardown); the proxy
// neighbor is removed from whatever uplink currently faces the pool.
func RemoveEIPIngress(ctx context.Context, r Runner, eip, poolGateway, uplinkHint string) error {
	if eip == "" {
		return fmt.Errorf("RemoveEIPIngress: eip required")
	}

	if _, err := r.Run(ctx, "ip", "route", "del", eip+"/32", "dev", NATTransitHostEnd); err != nil {
		slog.Debug("host: EIP route already absent", "eip", eip, "err", err)
	}

	if uplink, _, err := resolveUplink(ctx, r, poolGateway, uplinkHint); err != nil {
		slog.Debug("host: uplink detection failed on EIP teardown", "eip", eip, "err", err)
	} else if uplink != "" {
		if _, err := r.Run(ctx, "ip", "neigh", "del", "proxy", eip, "dev", uplink); err != nil {
			slog.Debug("host: proxy-ARP entry already absent", "eip", eip, "uplink", uplink, "err", err)
		}
	}

	for _, spec := range eipForwardRules(eip) {
		if _, err := r.Run(ctx, "iptables", natRuleArgs("-D", "filter", "FORWARD", spec)...); err != nil {
			slog.Debug("host: EIP FORWARD rule not present on delete", "eip", eip, "err", err)
		}
	}

	slog.Info("EIP ingress removed", "eip", eip)
	return nil
}
