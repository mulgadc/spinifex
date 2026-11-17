package policy

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/domains/network/topology"
)

const (
	// PeeringGuardPriority drops anything arriving over a peering link that is
	// not peer-to-local, ahead of every route policy.
	PeeringGuardPriority = 1400

	// peeringRouteBase puts every peering route above the drop gate (1100) and
	// the system-instance allow (1200). A route to D sits at base + 2·bits(D)
	// and its reroute one above, so ranks follow longest-prefix match.
	peeringRouteBase = 1300

	peeringRouteDropRole = "peering-route-drop"
	peeringRerouteRole   = "peering-reroute"
)

// PeeringGuards returns the guard on each end of spec's link, requester first.
func PeeringGuards(spec topology.PeeringLinkSpec) ([2]nbdb.LogicalRouterPolicy, error) {
	var out [2]nbdb.LogicalRouterPolicy
	for i, vpcID := range []string{spec.RequesterVPC, spec.AccepterVPC} {
		side, ok := spec.Side(vpcID)
		if !ok || !side.LocalCIDR.IsValid() || !side.PeerCIDR.IsValid() {
			return out, fmt.Errorf("peering guard %s: both VPC ranges required", spec.PcxID)
		}
		out[i] = nbdb.LogicalRouterPolicy{
			Priority: PeeringGuardPriority,
			Match: fmt.Sprintf(`inport == %q && (ip4.src != %s || ip4.dst != %s || ip6)`,
				side.LocalPort, side.PeerCIDR, side.LocalCIDR),
			Action:  "drop",
			Options: map[string]string{},
			ExternalIDs: map[string]string{
				ovn.ExternalIDPcxID: spec.PcxID,
				"spinifex:role":     ovn.PeeringGuardRole,
			},
		}
	}
	return out, nil
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
	prio := peeringRouteBase + 2*spec.Dest.Bits()
	ids := map[string]string{
		"spinifex:rtb_id":    spec.RouteTableID,
		"spinifex:subnet":    spec.SubnetID,
		"spinifex:route_dst": spec.Dest.String(),
		"spinifex:role":      peeringRouteDropRole,
	}
	rows := []nbdb.LogicalRouterPolicy{{
		Priority: prio, Match: peeringRouteMatch(spec.SubnetID, spec.Dest, spec.Exclude),
		Action: "drop", Options: map[string]string{}, ExternalIDs: ids,
	}}
	r := spec.Reroute
	if r == nil || !spec.Dest.Overlaps(r.PeerCIDR) {
		return rows
	}
	deliver := spec.Dest
	if r.PeerCIDR.Bits() > deliver.Bits() {
		deliver = r.PeerCIDR
	}
	nexthop := r.Nexthop.String()
	rows = append(rows, nbdb.LogicalRouterPolicy{
		Priority: prio + 1, Match: peeringRouteMatch(spec.SubnetID, deliver, spec.Exclude),
		Action: "reroute", Nexthop: &nexthop, Options: map[string]string{},
		ExternalIDs: map[string]string{
			ovn.ExternalIDPcxID:    r.PcxID,
			"spinifex:subnet":      spec.SubnetID,
			"spinifex:route_dst":   spec.Dest.String(),
			"spinifex:output_port": r.OutputPort,
			"spinifex:role":        peeringRerouteRole,
		},
	})
	return rows
}

func peeringRouteMatch(subnetID string, dst netip.Prefix, exclude []netip.Prefix) string {
	match := fmt.Sprintf(`inport == %q && ip4.dst == %s`, topology.SubnetRouterPort(subnetID), dst)
	for _, ex := range exclude {
		if ex.IsValid() && dst.Overlaps(ex) {
			match += fmt.Sprintf(` && ip4.dst != %s`, ex)
		}
	}
	return match
}

// PeeringRouteManager converges a VPC router's peering route policies.
type PeeringRouteManager interface {
	// SyncPeeringRoutes makes vpcID's peering route policies exactly want:
	// missing or drifted rows are written, and peering route rows not in want
	// are deleted. Guards are not route rows and are left alone.
	SyncPeeringRoutes(ctx context.Context, vpcID string, want []nbdb.LogicalRouterPolicy) error
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
	role := p.ExternalIDs["spinifex:role"]
	return role == peeringRouteDropRole || role == peeringRerouteRole
}

func (m *peeringRouteManager) SyncPeeringRoutes(ctx context.Context, vpcID string, want []nbdb.LogicalRouterPolicy) error {
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
			if peeringRowMatches(cur, &row) {
				continue
			}
			if err := m.ovn.DeleteLogicalRouterPolicy(ctx, router, row.Priority, row.Match); err != nil {
				return fmt.Errorf("delete drifted peering policy %q on %s: %w", row.Match, router, err)
			}
		}
		if err := m.ovn.AddLogicalRouterPolicy(ctx, router, &row); err != nil {
			return fmt.Errorf("add peering policy %q on %s: %w", row.Match, router, err)
		}
	}
	for k, cur := range have {
		if _, ok := wanted[k]; ok || !isPeeringRouteRow(cur) {
			continue
		}
		if err := m.ovn.DeleteLogicalRouterPolicy(ctx, router, cur.Priority, cur.Match); err != nil {
			return fmt.Errorf("delete stale peering policy %q on %s: %w", cur.Match, router, err)
		}
	}
	return nil
}

func peeringRowMatches(cur, want *nbdb.LogicalRouterPolicy) bool {
	if cur.Action != want.Action || !isPeeringRouteRow(cur) {
		return false
	}
	if (cur.Nexthop == nil) != (want.Nexthop == nil) || (cur.Nexthop != nil && *cur.Nexthop != *want.Nexthop) {
		return false
	}
	for _, k := range []string{ovn.ExternalIDPcxID, "spinifex:output_port", "spinifex:rtb_id", "spinifex:role"} {
		if cur.ExternalIDs[k] != want.ExternalIDs[k] {
			return false
		}
	}
	return true
}
