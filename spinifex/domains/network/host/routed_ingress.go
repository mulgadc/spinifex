package host

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// EnsureVPCIngressRoute installs the host route that makes a VPC's private IPs
// reachable in routed-NAT mode: traffic to the VPC CIDR enters OVN via the
// gateway LRP's transit IP on the spx-nat veth. Idempotent (route replace).
func EnsureVPCIngressRoute(ctx context.Context, r Runner, vpcCIDR, gwLrpIP string) error {
	if vpcCIDR == "" {
		return fmt.Errorf("EnsureVPCIngressRoute: vpcCIDR required")
	}
	if gwLrpIP == "" {
		return fmt.Errorf("EnsureVPCIngressRoute: gwLrpIP required")
	}
	if out, err := r.Run(ctx, "ip", "route", "replace", vpcCIDR,
		"via", gwLrpIP, "dev", NATTransitHostEnd); err != nil {
		return fmt.Errorf("install VPC ingress route %s via %s: %s: %w", vpcCIDR, gwLrpIP, string(out), err)
	}
	slog.Info("VPC ingress route installed", "vpc_cidr", vpcCIDR, "gw_lrp_ip", gwLrpIP)
	return nil
}

// VPCIngressRouteVia returns the gateway IP of the existing transit-veth host
// route for a VPC CIDR, or "" when none exists. Lets callers detect a
// duplicate-CIDR collision (two VPCs sharing e.g. 172.31.0.0/16) before
// replacing or deleting a route another VPC installed.
func VPCIngressRouteVia(ctx context.Context, r Runner, vpcCIDR string) (string, error) {
	if vpcCIDR == "" {
		return "", fmt.Errorf("VPCIngressRouteVia: vpcCIDR required")
	}
	out, err := r.Run(ctx, "ip", "route", "show", vpcCIDR, "dev", NATTransitHostEnd)
	if err != nil {
		return "", fmt.Errorf("show VPC ingress route %s: %s: %w", vpcCIDR, string(out), err)
	}
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "via" && i+1 < len(fields) {
			return fields[i+1], nil
		}
	}
	return "", nil
}

// RemoveVPCIngressRoute deletes the host route for a VPC CIDR on IGW detach.
// A missing route is not an error (idempotent teardown).
func RemoveVPCIngressRoute(ctx context.Context, r Runner, vpcCIDR string) error {
	if vpcCIDR == "" {
		return fmt.Errorf("RemoveVPCIngressRoute: vpcCIDR required")
	}
	if _, err := r.Run(ctx, "ip", "route", "del", vpcCIDR, "dev", NATTransitHostEnd); err != nil {
		slog.Debug("VPC ingress route already absent", "vpc_cidr", vpcCIDR, "err", err)
		return nil
	}
	slog.Info("VPC ingress route removed", "vpc_cidr", vpcCIDR)
	return nil
}
