package policy

import (
	"context"
	"fmt"
	"net/netip"
	"slices"

	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/domains/network/topology"
)

const (
	// PeeringGuardPriority drops anything arriving over a peering link that is
	// not peer-to-local, ahead of every route policy.
	PeeringGuardPriority = 1400

	// PeeringNoRoutePriority drops the peer range wherever no peering route
	// decides it, above the system-instance allow (1200) so an EIP instance's
	// allow never accepts the entry route.
	PeeringNoRoutePriority = 1250

	// peeringRouteBase puts every peering route above the drop gate (1100) and
	// the system-instance allow (1200). A route to D sits at base + 2·bits(D)
	// and its reroute one above, so ranks follow longest-prefix match.
	peeringRouteBase = 1300

	// PeeringRouteDropRole and PeeringRerouteRole are the roles of a peering
	// route's drop and of its delivery over the link.
	PeeringRouteDropRole = "peering-route-drop"
	PeeringRerouteRole   = "peering-reroute"
)

// PeeringGuard drops anything arriving on side's link port that is not
// peer-to-local IPv4.
func PeeringGuard(side topology.PeeringSide) nbdb.LogicalRouterPolicy {
	return nbdb.LogicalRouterPolicy{
		Priority: PeeringGuardPriority,
		Match: fmt.Sprintf(`inport == %q && (ip4.src != %s || ip4.dst != %s || ip6)`,
			side.LocalPort, side.PeerCIDR, side.LocalCIDR),
		Action:      "drop",
		Options:     map[string]string{},
		ExternalIDs: map[string]string{ovn.ExternalIDPcxID: side.PcxID, ovn.ExternalIDRole: ovn.PeeringGuardRole},
	}
}

// PeeringEntryRoute lets side's router, with no route covering the peer
// range, carry it to the policy stage; PeeringNoRouteDrop keeps it from
// forwarding wherever no peering route decides it.
func PeeringEntryRoute(side topology.PeeringSide) nbdb.LogicalRouterStaticRoute {
	port := side.LocalPort
	return nbdb.LogicalRouterStaticRoute{
		IPPrefix:    side.PeerCIDR.String(),
		Nexthop:     side.PeerAddr.String(),
		OutputPort:  &port,
		ExternalIDs: map[string]string{ovn.ExternalIDPcxID: side.PcxID},
	}
}

// PeeringNoRouteDrop drops side's peer range below every peering route.
func PeeringNoRouteDrop(side topology.PeeringSide) nbdb.LogicalRouterPolicy {
	return nbdb.LogicalRouterPolicy{
		Priority:    PeeringNoRoutePriority,
		Match:       fmt.Sprintf("ip4.dst == %s", side.PeerCIDR),
		Action:      "drop",
		Options:     map[string]string{},
		ExternalIDs: map[string]string{ovn.ExternalIDPcxID: side.PcxID, ovn.ExternalIDRole: ovn.PeeringNoRouteRole},
	}
}

// PeeringReroute delivers the part of a peering route's destination inside
// the peer's range over the link.
type PeeringReroute struct {
	PcxID      string
	Nexthop    netip.Addr
	OutputPort string
	PeerCIDR   netip.Prefix
}

// PeeringRouteSpec is one route D → pcx on one subnet the table governs.
// Exclude lists the VPC range and the more specific non-peering routes in D,
// which longest-prefix match sends elsewhere. Reroute is nil unless active.
type PeeringRouteSpec struct {
	RouteTableID string
	SubnetID     string
	Dest         netip.Prefix
	Exclude      []netip.Prefix
	Reroute      *PeeringReroute
}

// PeeringRouteRows compiles one route to its drop and, while it delivers, its
// reroute. Both rank by D, never by the intersection with the peer range.
func PeeringRouteRows(spec PeeringRouteSpec) []nbdb.LogicalRouterPolicy {
	rows := routeRows(spec.RouteTableID, spec.SubnetID, spec.Dest, spec.Exclude, PeeringRouteDropRole)
	r := spec.Reroute
	if r == nil || !spec.Dest.Overlaps(r.PeerCIDR) {
		return rows
	}
	deliver := spec.Dest
	if r.PeerCIDR.Bits() > deliver.Bits() {
		deliver = r.PeerCIDR
	}
	return append(rows, rerouteRow(spec.SubnetID, spec.Dest, deliver, spec.Exclude, r.Nexthop, map[string]string{
		ovn.ExternalIDPcxID:    r.PcxID,
		"spinifex:output_port": r.OutputPort,
		ovn.ExternalIDRole:     PeeringRerouteRole,
	}))
}

// routeRows is the drop of a route to dest on one subnet.
func routeRows(rtbID, subnetID string, dest netip.Prefix, exclude []netip.Prefix, role string) []nbdb.LogicalRouterPolicy {
	return []nbdb.LogicalRouterPolicy{{
		Priority: routePriority(dest), Match: peeringRouteMatch(subnetID, dest, exclude),
		Action: "drop", Options: map[string]string{},
		ExternalIDs: map[string]string{
			"spinifex:rtb_id":    rtbID,
			"spinifex:subnet":    subnetID,
			"spinifex:route_dst": dest.String(),
			ovn.ExternalIDRole:   role,
		},
	}}
}

// rerouteRow delivers deliver, the part of a route to dest that leaves over a
// link, one above the route's drop. ids carry the owner, port and role.
func rerouteRow(subnetID string, dest, deliver netip.Prefix, exclude []netip.Prefix, nexthop netip.Addr, ids map[string]string) nbdb.LogicalRouterPolicy {
	ids["spinifex:subnet"] = subnetID
	ids["spinifex:route_dst"] = dest.String()
	// nexthops, not nexthop: OVN 26.03 ignores the deprecated column, which
	// would leave only the drop below.
	return nbdb.LogicalRouterPolicy{
		Priority: routePriority(dest) + 1, Match: peeringRouteMatch(subnetID, deliver, exclude),
		Action: "reroute", Nexthops: []string{nexthop.String()}, Options: map[string]string{},
		ExternalIDs: ids,
	}
}

func routePriority(dest netip.Prefix) int { return peeringRouteBase + 2*dest.Bits() }

func peeringRouteMatch(subnetID string, dst netip.Prefix, exclude []netip.Prefix) string {
	match := fmt.Sprintf(`inport == %q && ip4.dst == %s`, topology.SubnetRouterPort(subnetID), dst)
	for _, ex := range exclude {
		if ex.IsValid() && dst.Overlaps(ex) {
			match += fmt.Sprintf(` && ip4.dst != %s`, ex)
		}
	}
	return match
}

// PeeringRouteManager converges a VPC router's peering and Transit Gateway
// route policies, each kind on its own so a failed read of one leaves the other.
type PeeringRouteManager interface {
	// SyncPeeringRoutes makes vpcID's peering route policies exactly want:
	// missing or drifted rows are written, and peering route rows not in want
	// are deleted. Guards are not route rows and are left alone.
	SyncPeeringRoutes(ctx context.Context, vpcID string, want []nbdb.LogicalRouterPolicy) error
	// SyncTransitGatewayRoutes is SyncPeeringRoutes for Transit Gateway route rows.
	SyncTransitGatewayRoutes(ctx context.Context, vpcID string, want []nbdb.LogicalRouterPolicy) error
}

type peeringRouteManager struct {
	ovn ovn.Client
}

var _ PeeringRouteManager = (*peeringRouteManager)(nil)

// NewPeeringRouteManager constructs a PeeringRouteManager backed by client.
func NewPeeringRouteManager(client ovn.Client) PeeringRouteManager {
	return &peeringRouteManager{ovn: client}
}

func isPeeringRouteRow(p *nbdb.LogicalRouterPolicy) bool {
	role := p.ExternalIDs[ovn.ExternalIDRole]
	return role == PeeringRouteDropRole || role == PeeringRerouteRole
}

func (m *peeringRouteManager) SyncPeeringRoutes(ctx context.Context, vpcID string, want []nbdb.LogicalRouterPolicy) error {
	return m.syncRouteRows(ctx, vpcID, want, isPeeringRouteRow)
}

func (m *peeringRouteManager) SyncTransitGatewayRoutes(ctx context.Context, vpcID string, want []nbdb.LogicalRouterPolicy) error {
	return m.syncRouteRows(ctx, vpcID, want, isTransitGatewayRouteRow)
}

// syncRouteRows makes the route rows of one kind, those isRow reports, exactly want.
func (m *peeringRouteManager) syncRouteRows(ctx context.Context, vpcID string, want []nbdb.LogicalRouterPolicy, isRow func(*nbdb.LogicalRouterPolicy) bool) error {
	router := topology.VPCRouter(vpcID)
	existing, err := m.ovn.ListLogicalRouterPolicies(ctx, router)
	if err != nil {
		return fmt.Errorf("list LR policies on %s: %w", router, err)
	}
	type key struct {
		prio  int
		match string
	}
	have := make(map[key]*nbdb.LogicalRouterPolicy, len(existing))
	for i := range existing {
		have[key{existing[i].Priority, existing[i].Match}] = &existing[i]
	}
	wanted := make(map[key]struct{}, len(want))
	for i := range want {
		row := want[i]
		k := key{row.Priority, row.Match}
		wanted[k] = struct{}{}
		if cur, ok := have[k]; ok {
			if routeRowMatches(cur, &row, isRow) {
				continue
			}
			// Rewritten in place: a delete then add would leave the destination
			// to the default route if the add failed.
			if err := m.ovn.UpdateLogicalRouterPolicy(ctx, router, &row); err != nil {
				return fmt.Errorf("update drifted peering policy %q on %s: %w", row.Match, router, err)
			}
			continue
		}
		if err := m.ovn.AddLogicalRouterPolicy(ctx, router, &row); err != nil {
			return fmt.Errorf("add peering policy %q on %s: %w", row.Match, router, err)
		}
	}
	for k, cur := range have {
		if _, ok := wanted[k]; ok || !isRow(cur) {
			continue
		}
		if err := m.ovn.DeleteLogicalRouterPolicy(ctx, router, cur.Priority, cur.Match); err != nil {
			return fmt.Errorf("delete stale peering policy %q on %s: %w", cur.Match, router, err)
		}
	}
	return nil
}

func routeRowMatches(cur, want *nbdb.LogicalRouterPolicy, isRow func(*nbdb.LogicalRouterPolicy) bool) bool {
	if cur.Action != want.Action || !isRow(cur) {
		return false
	}
	if (cur.Nexthop == nil) != (want.Nexthop == nil) || (cur.Nexthop != nil && *cur.Nexthop != *want.Nexthop) {
		return false
	}
	if !slices.Equal(cur.Nexthops, want.Nexthops) {
		return false
	}
	for _, k := range []string{ovn.ExternalIDPcxID, ovn.ExternalIDTGWAttachmentID, "spinifex:output_port", "spinifex:rtb_id", ovn.ExternalIDRole} {
		if cur.ExternalIDs[k] != want.ExternalIDs[k] {
			return false
		}
	}
	return true
}
