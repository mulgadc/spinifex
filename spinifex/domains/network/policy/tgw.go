package policy

import (
	"fmt"
	"net/netip"

	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/domains/network/topology"
)

// Roles of the rows a Transit Gateway attachment owns, and of its route rows.
const (
	TransitGatewayGuardRole       = "tgw-guard"
	TransitGatewayNoRouteRole     = "tgw-no-route"
	TransitGatewayEntryRole       = "tgw-entry"
	TransitGatewayPropagationRole = "tgw-propagation"
	TransitGatewayRouteDropRole   = "tgw-route-drop"
	TransitGatewayRerouteRole     = ovn.TransitGatewayRerouteRole
)

func transitGatewayIDs(spec topology.TransitGatewayLinkSpec, role string) map[string]string {
	return map[string]string{ovn.ExternalIDTGWAttachmentID: spec.AttachmentID, ovn.ExternalIDRole: role}
}

// TransitGatewayGuard drops anything arriving on the VPC's link port that is
// not IPv4 to the VPC's range. Sources are any attached VPC.
func TransitGatewayGuard(spec topology.TransitGatewayLinkSpec) nbdb.LogicalRouterPolicy {
	return nbdb.LogicalRouterPolicy{
		Priority: PeeringGuardPriority,
		Match: fmt.Sprintf(`inport == %q && (ip4.dst != %s || ip6)`,
			topology.TransitGatewayVPCPort(spec.AttachmentID), spec.VPCCIDR),
		Action:      "drop",
		Options:     map[string]string{},
		ExternalIDs: transitGatewayIDs(spec, TransitGatewayGuardRole),
	}
}

// TransitGatewayNoRoute drops whatever an entry route sends to the link and no
// reroute claims: traffic from subnets whose tables have no route to the
// Transit Gateway, or whose route does not deliver.
func TransitGatewayNoRoute(spec topology.TransitGatewayLinkSpec) nbdb.LogicalRouterPolicy {
	return nbdb.LogicalRouterPolicy{
		Priority:    PeeringNoRoutePriority,
		Match:       fmt.Sprintf(`outport == %q`, topology.TransitGatewayVPCPort(spec.AttachmentID)),
		Action:      "drop",
		Options:     map[string]string{},
		ExternalIDs: transitGatewayIDs(spec, TransitGatewayNoRouteRole),
	}
}

// TransitGatewayEntryRoute lets the VPC router, with no route covering dest,
// carry it to the policy stage, where the route rows decide it.
func TransitGatewayEntryRoute(spec topology.TransitGatewayLinkSpec, dest netip.Prefix) nbdb.LogicalRouterStaticRoute {
	port := topology.TransitGatewayVPCPort(spec.AttachmentID)
	ids := transitGatewayIDs(spec, TransitGatewayEntryRole)
	ids["spinifex:route_dst"] = dest.String()
	return nbdb.LogicalRouterStaticRoute{IPPrefix: dest.String(), Nexthop: spec.TGWAddr.String(), OutputPort: &port, ExternalIDs: ids}
}

// TransitGatewayPropagation is the Transit Gateway router's route to the
// VPC's range over the link: the propagated route of its default table.
func TransitGatewayPropagation(spec topology.TransitGatewayLinkSpec) nbdb.LogicalRouterStaticRoute {
	port := topology.TransitGatewayPort(spec.AttachmentID)
	return nbdb.LogicalRouterStaticRoute{
		IPPrefix: spec.VPCCIDR.String(), Nexthop: spec.VPCAddr.String(), OutputPort: &port,
		ExternalIDs: transitGatewayIDs(spec, TransitGatewayPropagationRole),
	}
}

// TransitGatewayReroute delivers a route's destination over an attachment's link.
type TransitGatewayReroute struct {
	AttachmentID string
	Nexthop      netip.Addr
	OutputPort   string
}

// TransitGatewayRouteSpec is one route D → tgw on one subnet the table
// governs. Exclude is as for peering. Reroute is nil unless the route delivers.
type TransitGatewayRouteSpec struct {
	RouteTableID string
	SubnetID     string
	Dest         netip.Prefix
	Exclude      []netip.Prefix
	Reroute      *TransitGatewayReroute
}

// TransitGatewayRouteRows compiles one route to its drop and, while it
// delivers, a reroute of all of D. They rank with peering's route rows.
func TransitGatewayRouteRows(spec TransitGatewayRouteSpec) []nbdb.LogicalRouterPolicy {
	rows := routeRows(spec.RouteTableID, spec.SubnetID, spec.Dest, spec.Exclude, TransitGatewayRouteDropRole)
	r := spec.Reroute
	if r == nil {
		return rows
	}
	return append(rows, rerouteRow(spec.SubnetID, spec.Dest, spec.Dest, spec.Exclude, r.Nexthop, map[string]string{
		ovn.ExternalIDTGWAttachmentID: r.AttachmentID,
		"spinifex:output_port":        r.OutputPort,
		ovn.ExternalIDRole:            TransitGatewayRerouteRole,
	}))
}

func isTransitGatewayRouteRow(p *nbdb.LogicalRouterPolicy) bool {
	role := p.ExternalIDs[ovn.ExternalIDRole]
	return role == TransitGatewayRouteDropRole || role == TransitGatewayRerouteRole
}
