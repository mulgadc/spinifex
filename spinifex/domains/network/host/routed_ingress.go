package host

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
)

// VPCIngressRouteProto tags every VPC ingress route this package installs, so
// the routes it may prune are told apart from the transit /24 and the EIP /32s
// that share the spx-nat veth. Unassigned in iproute2's rt_protos.
const VPCIngressRouteProto = "201"

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
		"via", gwLrpIP, "dev", NATTransitHostEnd, "proto", VPCIngressRouteProto); err != nil {
		return fmt.Errorf("install VPC ingress route %s via %s: %s: %w", vpcCIDR, gwLrpIP, string(out), err)
	}
	slog.Info("VPC ingress route installed", "vpc_cidr", vpcCIDR, "gw_lrp_ip", gwLrpIP)
	return nil
}

// ListVPCIngressRoutes returns the VPC ingress routes on this host, VPC CIDR to
// gateway LRP transit IP. Only routes carrying VPCIngressRouteProto are listed.
func ListVPCIngressRoutes(ctx context.Context, r Runner) (map[string]string, error) {
	out, err := r.Run(ctx, "ip", "-o", "route", "show", "dev", NATTransitHostEnd, "proto", VPCIngressRouteProto)
	if err != nil {
		return nil, fmt.Errorf("list VPC ingress routes: %s: %w", string(out), err)
	}
	routes := map[string]string{}
	for line := range strings.SplitSeq(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		dst, err := netip.ParsePrefix(fields[0])
		if err != nil {
			slog.Warn("VPC ingress route list: unparseable destination", "line", strings.TrimSpace(line))
			continue
		}
		via := ""
		for i, f := range fields {
			if f == "via" && i+1 < len(fields) {
				via = fields[i+1]
			}
		}
		routes[dst.Masked().String()] = via
	}
	return routes, nil
}

// RemoveVPCIngressRoute deletes the host route for a VPC CIDR. A missing route
// is not an error (idempotent teardown).
func RemoveVPCIngressRoute(ctx context.Context, r Runner, vpcCIDR string) error {
	if vpcCIDR == "" {
		return fmt.Errorf("RemoveVPCIngressRoute: vpcCIDR required")
	}
	if _, err := r.Run(ctx, "ip", "route", "del", vpcCIDR, "dev", NATTransitHostEnd, "proto", VPCIngressRouteProto); err != nil {
		slog.Debug("VPC ingress route already absent", "vpc_cidr", vpcCIDR, "err", err)
		return nil
	}
	slog.Info("VPC ingress route removed", "vpc_cidr", vpcCIDR)
	return nil
}

// VPCIngressRoutes is this host's VPC ingress route table behind a Runner.
type VPCIngressRoutes struct {
	Runner Runner
}

// List is ListVPCIngressRoutes.
func (v VPCIngressRoutes) List(ctx context.Context) (map[string]string, error) {
	return ListVPCIngressRoutes(ctx, v.Runner)
}

// Ensure is EnsureVPCIngressRoute.
func (v VPCIngressRoutes) Ensure(ctx context.Context, vpcCIDR, gwLrpIP string) error {
	return EnsureVPCIngressRoute(ctx, v.Runner, vpcCIDR, gwLrpIP)
}

// Remove is RemoveVPCIngressRoute.
func (v VPCIngressRoutes) Remove(ctx context.Context, vpcCIDR string) error {
	return RemoveVPCIngressRoute(ctx, v.Runner, vpcCIDR)
}
