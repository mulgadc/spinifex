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
	// SubnetEgressPriorityDrop sits above both reroute priorities; a subnet
	// whose effective route table lacks 0.0.0.0/0 gets a drop policy at this
	// priority to override the VPC LR's router-wide default static route
	// (which is required so lr_in_ip_routing doesn't drop at priority 0
	// before lr_in_policy fires).
	SubnetEgressPriorityDrop  = 1100
	SubnetEgressPriorityIGW   = 1000
	SubnetEgressPriorityNATGW = 900

	// SystemInstanceEgressPriority sits above the drop gate (1100) so a single
	// system instance (e.g. an EKS K3s server VM) egresses even when its subnet
	// is otherwise drop-gated. The policy is scoped to the instance's /32
	// source, so peer instances in the same subnet are unaffected — they still
	// hit the 1100 drop (or the subnet's own 1000 reroute where one exists).
	SystemInstanceEgressPriority = 1200
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
// Nexthop, egressing through OutputPort. ExcludeCIDRs are appended as
// `&& ip4.dst != <cidr>` clauses so in-VPC traffic skips the reroute and
// follows the directly-connected route instead — without this, NATGW egress
// policies on a `0.0.0.0/0` prefix would intercept return traffic from
// peer subnets (bastion -> private SSH) and forward it to the public IP.
type SubnetEgressSpec struct {
	SubnetID     string
	Prefix       netip.Prefix
	Nexthop      string
	OutputPort   string
	Priority     int
	ExcludeCIDRs []netip.Prefix
}

// SystemInstanceEgressSpec describes a per-instance egress override installed
// as an OVN Logical_Router_Policy at SystemInstanceEgressPriority. The match
// adds an `ip4.src == <SrcIP>/32` clause to the subnet-egress shape so the
// reroute is confined to a single system instance; peer instances in the same
// subnet are untouched. Action is "reroute" via Nexthop out OutputPort.
type SystemInstanceEgressSpec struct {
	SubnetID     string
	SrcIP        netip.Addr
	Prefix       netip.Prefix
	Nexthop      string
	OutputPort   string
	ExcludeCIDRs []netip.Prefix
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
	DeleteSubnetEgress(ctx context.Context, vpcID, subnetID string, prefix netip.Prefix, excludeCIDRs []netip.Prefix) error
	AddSubnetEgressDrop(ctx context.Context, vpcID string, spec SubnetEgressDropSpec) error
	DeleteSubnetEgressDrop(ctx context.Context, vpcID, subnetID string, prefix netip.Prefix, excludeCIDRs []netip.Prefix) error
	AddSystemInstanceEgress(ctx context.Context, vpcID string, spec SystemInstanceEgressSpec) error
	DeleteSystemInstanceEgress(ctx context.Context, vpcID, subnetID string, srcIP netip.Addr, prefix netip.Prefix, excludeCIDRs []netip.Prefix) error
}

// SubnetEgressDropSpec describes a per-subnet egress DROP override installed
// as an OVN Logical_Router_Policy at SubnetEgressPriorityDrop. Match shape
// mirrors SubnetEgressSpec (inport == rtr-<subnet> && ip4.dst == prefix with
// optional ip4.dst != <exclude> clauses) but the action is "drop" with no
// nexthop/output port. Used to gate subnets whose effective route table
// lacks a 0.0.0.0/0 entry; the policy fires after lr_in_ip_routing has
// already matched the VPC LR's router-wide default static route, killing
// the packet before egress.
type SubnetEgressDropSpec struct {
	SubnetID     string
	Prefix       netip.Prefix
	ExcludeCIDRs []netip.Prefix
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
	match := subnetEgressMatch(spec.SubnetID, spec.Prefix, spec.ExcludeCIDRs)

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

func (m *routeManager) DeleteSubnetEgress(ctx context.Context, vpcID, subnetID string, prefix netip.Prefix, excludeCIDRs []netip.Prefix) error {
	router := topology.VPCRouter(vpcID)
	match := subnetEgressMatch(subnetID, prefix, excludeCIDRs)
	if err := m.ovn.DeleteLogicalRouterPolicy(ctx, router, SubnetEgressPriorityIGW, match); err != nil {
		return fmt.Errorf("delete IGW LR policy %q on %s: %w", match, router, err)
	}
	if err := m.ovn.DeleteLogicalRouterPolicy(ctx, router, SubnetEgressPriorityNATGW, match); err != nil {
		return fmt.Errorf("delete NATGW LR policy %q on %s: %w", match, router, err)
	}
	return nil
}

func (m *routeManager) AddSubnetEgressDrop(ctx context.Context, vpcID string, spec SubnetEgressDropSpec) error {
	if spec.SubnetID == "" {
		return fmt.Errorf("subnet egress drop: SubnetID required")
	}
	if !spec.Prefix.IsValid() {
		return fmt.Errorf("subnet egress drop: Prefix required")
	}
	router := topology.VPCRouter(vpcID)
	match := subnetEgressMatch(spec.SubnetID, spec.Prefix, spec.ExcludeCIDRs)

	existing, err := m.ovn.FindLogicalRouterPolicy(ctx, router, SubnetEgressPriorityDrop, match)
	if err != nil {
		return fmt.Errorf("find LR drop policy %q on %s: %w", match, router, err)
	}
	if existing != nil {
		if dropPolicyMatches(existing) {
			return nil
		}
		if err := m.ovn.DeleteLogicalRouterPolicy(ctx, router, SubnetEgressPriorityDrop, match); err != nil {
			return fmt.Errorf("delete drifted LR drop policy %q on %s: %w", match, router, err)
		}
	}

	row := &nbdb.LogicalRouterPolicy{
		Priority: SubnetEgressPriorityDrop,
		Match:    match,
		Action:   "drop",
		Options:  map[string]string{},
		ExternalIDs: map[string]string{
			"spinifex:subnet": spec.SubnetID,
			"spinifex:gate":   "drop",
		},
	}
	if err := m.ovn.AddLogicalRouterPolicy(ctx, router, row); err != nil {
		return fmt.Errorf("add LR drop policy %q on %s: %w", match, router, err)
	}
	return nil
}

func (m *routeManager) DeleteSubnetEgressDrop(ctx context.Context, vpcID, subnetID string, prefix netip.Prefix, excludeCIDRs []netip.Prefix) error {
	router := topology.VPCRouter(vpcID)
	match := subnetEgressMatch(subnetID, prefix, excludeCIDRs)
	if err := m.ovn.DeleteLogicalRouterPolicy(ctx, router, SubnetEgressPriorityDrop, match); err != nil {
		return fmt.Errorf("delete LR drop policy %q on %s: %w", match, router, err)
	}
	return nil
}

func (m *routeManager) AddSystemInstanceEgress(ctx context.Context, vpcID string, spec SystemInstanceEgressSpec) error {
	if spec.SubnetID == "" {
		return fmt.Errorf("system instance egress: SubnetID required")
	}
	if !spec.SrcIP.IsValid() {
		return fmt.Errorf("system instance egress: SrcIP required")
	}
	if spec.OutputPort == "" {
		return fmt.Errorf("system instance egress: OutputPort required (ovn-northd drops policy reroute otherwise)")
	}
	router := topology.VPCRouter(vpcID)
	match := systemInstanceEgressMatch(spec.SubnetID, spec.SrcIP, spec.Prefix, spec.ExcludeCIDRs)

	existing, err := m.ovn.FindLogicalRouterPolicy(ctx, router, SystemInstanceEgressPriority, match)
	if err != nil {
		return fmt.Errorf("find LR policy %q on %s: %w", match, router, err)
	}
	if existing != nil {
		if existing.Action == "reroute" && existing.Nexthop != nil && *existing.Nexthop == spec.Nexthop &&
			existing.ExternalIDs["spinifex:output_port"] == spec.OutputPort {
			return nil
		}
		if err := m.ovn.DeleteLogicalRouterPolicy(ctx, router, SystemInstanceEgressPriority, match); err != nil {
			return fmt.Errorf("delete drifted LR policy %q on %s: %w", match, router, err)
		}
	}

	nexthop := spec.Nexthop
	row := &nbdb.LogicalRouterPolicy{
		Priority: SystemInstanceEgressPriority,
		Match:    match,
		Action:   "reroute",
		Nexthop:  &nexthop,
		Options:  map[string]string{},
		ExternalIDs: map[string]string{
			"spinifex:subnet":      spec.SubnetID,
			"spinifex:src_ip":      spec.SrcIP.String(),
			"spinifex:output_port": spec.OutputPort,
			"spinifex:role":        "system-instance-egress",
		},
	}
	if err := m.ovn.AddLogicalRouterPolicy(ctx, router, row); err != nil {
		return fmt.Errorf("add LR policy %q -> %s on %s: %w", match, spec.Nexthop, router, err)
	}
	return nil
}

func (m *routeManager) DeleteSystemInstanceEgress(ctx context.Context, vpcID, subnetID string, srcIP netip.Addr, prefix netip.Prefix, excludeCIDRs []netip.Prefix) error {
	router := topology.VPCRouter(vpcID)
	match := systemInstanceEgressMatch(subnetID, srcIP, prefix, excludeCIDRs)
	if err := m.ovn.DeleteLogicalRouterPolicy(ctx, router, SystemInstanceEgressPriority, match); err != nil {
		return fmt.Errorf("delete system instance egress LR policy %q on %s: %w", match, router, err)
	}
	return nil
}

func subnetEgressMatch(subnetID string, prefix netip.Prefix, excludeCIDRs []netip.Prefix) string {
	match := fmt.Sprintf(`inport == %q && ip4.dst == %s`, topology.SubnetRouterPort(subnetID), prefix.String())
	for _, ex := range excludeCIDRs {
		if !ex.IsValid() {
			continue
		}
		match += fmt.Sprintf(` && ip4.dst != %s`, ex.String())
	}
	return match
}

func systemInstanceEgressMatch(subnetID string, srcIP netip.Addr, prefix netip.Prefix, excludeCIDRs []netip.Prefix) string {
	match := fmt.Sprintf(`inport == %q && ip4.src == %s/32 && ip4.dst == %s`,
		topology.SubnetRouterPort(subnetID), srcIP.String(), prefix.String())
	for _, ex := range excludeCIDRs {
		if !ex.IsValid() {
			continue
		}
		match += fmt.Sprintf(` && ip4.dst != %s`, ex.String())
	}
	return match
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

func dropPolicyMatches(existing *nbdb.LogicalRouterPolicy) bool {
	return existing.Action == "drop"
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
