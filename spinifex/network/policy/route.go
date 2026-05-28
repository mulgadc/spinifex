package policy

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/mulgadc/spinifex/spinifex/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

const (
	defaultRoutePrefix = "0.0.0.0/0"

	// SubnetEgressPriorityIGW is the priority assigned to per-subnet default
	// egress policies routing via an IGW. NATGW egress sits at a different
	// priority so the route precedence is deterministic when both apply.
	SubnetEgressPriorityIGW   = 1000
	SubnetEgressPriorityNATGW = 900
)

// RouteSpec is a static route on a VPC's LogicalRouter. OutputPort is
// required when Nexthop isn't directly-connected (e.g. IGW default route on
// 169.254.0.1/30) — ovn-northd silently drops the route from SB otherwise.
type RouteSpec struct {
	Prefix     netip.Prefix
	Nexthop    string
	OutputPort string
}

// SubnetEgressSpec describes a per-subnet egress override installed as an
// OVN Logical_Router_Policy. The match is built from SubnetID (inport ==
// "rtr-<subnetID>") AND ip4.dst == Prefix; the action is "reroute" via
// Nexthop, egressing through OutputPort.
type SubnetEgressSpec struct {
	SubnetID   string
	Prefix     netip.Prefix
	Nexthop    string
	OutputPort string
	Priority   int
}

// RouteManager owns static routes on VPC LogicalRouters. Adds are
// idempotent (no-op on match, delete-then-add on drift) so the reconciler
// can replay safely.
type RouteManager interface {
	AddDefaultRoute(ctx context.Context, vpcID, nexthop, outputPort string) error
	DeleteDefaultRoute(ctx context.Context, vpcID string) error
	AddStaticRoute(ctx context.Context, vpcID string, route RouteSpec) error
	DeleteStaticRoute(ctx context.Context, vpcID string, prefix netip.Prefix) error
	AddSubnetEgress(ctx context.Context, vpcID string, spec SubnetEgressSpec) error
	DeleteSubnetEgress(ctx context.Context, vpcID, subnetID string, prefix netip.Prefix) error
}

type routeManager struct {
	ovn ovn.Client
}

var _ RouteManager = (*routeManager)(nil)

// NewRouteManager constructs a RouteManager backed by client.
func NewRouteManager(client ovn.Client) RouteManager {
	return &routeManager{ovn: client}
}

func (m *routeManager) AddDefaultRoute(ctx context.Context, vpcID, nexthop, outputPort string) error {
	prefix, err := netip.ParsePrefix(defaultRoutePrefix)
	if err != nil {
		return fmt.Errorf("parse default route prefix: %w", err)
	}
	return m.AddStaticRoute(ctx, vpcID, RouteSpec{
		Prefix:     prefix,
		Nexthop:    nexthop,
		OutputPort: outputPort,
	})
}

func (m *routeManager) DeleteDefaultRoute(ctx context.Context, vpcID string) error {
	return m.deleteByPrefixString(ctx, vpcID, defaultRoutePrefix)
}

func (m *routeManager) AddStaticRoute(ctx context.Context, vpcID string, route RouteSpec) error {
	router := topology.VPCRouter(vpcID)
	prefixStr := route.Prefix.String()

	existing, err := m.ovn.FindStaticRoute(ctx, router, prefixStr)
	if err != nil {
		return fmt.Errorf("find static route %s on %s: %w", prefixStr, router, err)
	}
	if existing != nil {
		if routeMatches(existing, route) {
			return nil
		}
		if err := m.ovn.DeleteStaticRoute(ctx, router, prefixStr); err != nil {
			return fmt.Errorf("delete drifted static route %s on %s: %w", prefixStr, router, err)
		}
	}

	row := &nbdb.LogicalRouterStaticRoute{
		IPPrefix: prefixStr,
		Nexthop:  route.Nexthop,
	}
	if route.OutputPort != "" {
		op := route.OutputPort
		row.OutputPort = &op
	}
	if err := m.ovn.AddStaticRoute(ctx, router, row); err != nil {
		return fmt.Errorf("add static route %s -> %s on %s: %w", prefixStr, route.Nexthop, router, err)
	}
	return nil
}

func (m *routeManager) DeleteStaticRoute(ctx context.Context, vpcID string, prefix netip.Prefix) error {
	return m.deleteByPrefixString(ctx, vpcID, prefix.String())
}

// deleteByPrefixString returns nil when the route is already absent (every
// Delete* entrypoint needs this for fire-and-forget event flows).
func (m *routeManager) deleteByPrefixString(ctx context.Context, vpcID, prefix string) error {
	router := topology.VPCRouter(vpcID)
	existing, err := m.ovn.FindStaticRoute(ctx, router, prefix)
	if err != nil {
		return fmt.Errorf("find static route %s on %s: %w", prefix, router, err)
	}
	if existing == nil {
		return nil
	}
	if err := m.ovn.DeleteStaticRoute(ctx, router, prefix); err != nil {
		return fmt.Errorf("delete static route %s on %s: %w", prefix, router, err)
	}
	return nil
}

func (m *routeManager) AddSubnetEgress(ctx context.Context, vpcID string, spec SubnetEgressSpec) error {
	if spec.SubnetID == "" {
		return fmt.Errorf("subnet egress: SubnetID required")
	}
	if spec.OutputPort == "" {
		return fmt.Errorf("subnet egress: OutputPort required (ovn-northd drops policy reroute otherwise)")
	}
	if spec.Priority == 0 {
		return fmt.Errorf("subnet egress: Priority required")
	}
	router := topology.VPCRouter(vpcID)
	match := subnetEgressMatch(spec.SubnetID, spec.Prefix)

	existing, err := m.ovn.FindLogicalRouterPolicy(ctx, router, spec.Priority, match)
	if err != nil {
		return fmt.Errorf("find LR policy %q on %s: %w", match, router, err)
	}
	if existing != nil {
		if policyMatches(existing, spec) {
			return nil
		}
		if err := m.ovn.DeleteLogicalRouterPolicy(ctx, router, spec.Priority, match); err != nil {
			return fmt.Errorf("delete drifted LR policy %q on %s: %w", match, router, err)
		}
	}

	nexthop := spec.Nexthop
	row := &nbdb.LogicalRouterPolicy{
		Priority: spec.Priority,
		Match:    match,
		Action:   "reroute",
		Nexthop:  &nexthop,
		Options:  map[string]string{},
		ExternalIDs: map[string]string{
			"spinifex:subnet":      spec.SubnetID,
			"spinifex:output_port": spec.OutputPort,
		},
	}
	if err := m.ovn.AddLogicalRouterPolicy(ctx, router, row); err != nil {
		return fmt.Errorf("add LR policy %q -> %s on %s: %w", match, spec.Nexthop, router, err)
	}
	return nil
}

func (m *routeManager) DeleteSubnetEgress(ctx context.Context, vpcID, subnetID string, prefix netip.Prefix) error {
	router := topology.VPCRouter(vpcID)
	match := subnetEgressMatch(subnetID, prefix)
	if err := m.ovn.DeleteLogicalRouterPolicy(ctx, router, SubnetEgressPriorityIGW, match); err != nil {
		return fmt.Errorf("delete IGW LR policy %q on %s: %w", match, router, err)
	}
	if err := m.ovn.DeleteLogicalRouterPolicy(ctx, router, SubnetEgressPriorityNATGW, match); err != nil {
		return fmt.Errorf("delete NATGW LR policy %q on %s: %w", match, router, err)
	}
	return nil
}

func subnetEgressMatch(subnetID string, prefix netip.Prefix) string {
	return fmt.Sprintf(`inport == %q && ip4.dst == %s`, topology.SubnetRouterPort(subnetID), prefix.String())
}

func policyMatches(existing *nbdb.LogicalRouterPolicy, want SubnetEgressSpec) bool {
	if existing.Action != "reroute" {
		return false
	}
	if existing.Nexthop == nil || *existing.Nexthop != want.Nexthop {
		return false
	}
	if existing.ExternalIDs["spinifex:output_port"] != want.OutputPort {
		return false
	}
	return true
}

func routeMatches(existing *nbdb.LogicalRouterStaticRoute, want RouteSpec) bool {
	if existing.Nexthop != want.Nexthop {
		return false
	}
	switch {
	case existing.OutputPort == nil && want.OutputPort == "":
		return true
	case existing.OutputPort == nil || want.OutputPort == "":
		return false
	default:
		return *existing.OutputPort == want.OutputPort
	}
}
