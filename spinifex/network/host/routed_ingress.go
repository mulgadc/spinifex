package host

import (
	"context"
	"fmt"
	"log/slog"
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
