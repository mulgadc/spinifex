package reconcile

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"maps"
	"net/netip"
	"slices"
	"strings"

	ec2projection "github.com/mulgadc/spinifex/spinifex/domains/ec2/projection"
	ec2routetable "github.com/mulgadc/spinifex/spinifex/domains/ec2/routetable"
	"github.com/mulgadc/spinifex/spinifex/domains/network/topology"
	"github.com/nats-io/nats.go/jetstream"
)

// TGWRouterIntent is a pending or available Transit Gateway: its router
// exists in every AZ's network.
type TGWRouterIntent struct {
	AccountID  string
	Generation int64
	Pending    bool
}

// TGWLinkIntent is a pending or available attachment of a local VPC that holds
// a link. Holds is set when it carries its VPC's range on the Transit Gateway.
type TGWLinkIntent struct {
	Spec       topology.TransitGatewayLinkSpec
	AccountID  string
	Generation int64
	Available  bool
	Holds      bool
	// AZs are the zones of its subnets; only sources there reach the link.
	AZs map[string]struct{}
}

// TGWOwner names the account and record a hook reports against.
type TGWOwner struct {
	AccountID string
	ID        string
}

// TGWLinkTeardownIntent is an attachment whose link must go.
type TGWLinkTeardownIntent struct {
	AccountID        string
	TransitGatewayID string
	VPCID            string
}

// TGWRouteIntent is one route Dest → tgw on one subnet its table governs.
type TGWRouteIntent struct {
	VPCID            string
	RouteTableID     string
	SubnetID         string
	Dest             netip.Prefix
	Exclude          []netip.Prefix
	TransitGatewayID string
}

// TransitGatewayIntent is the Transit Gateway part of IntentState. Incomplete
// is set when a bucket or the VPC key set could not be read; the stages then
// change nothing, so a failed read never tears a link down.
type TransitGatewayIntent struct {
	TGWRouters       map[string]TGWRouterIntent
	TGWTeardowns     map[string]TGWOwner // by Transit Gateway ID
	TGWLinks         map[string]TGWLinkIntent
	TGWLinkTeardowns map[string]TGWLinkTeardownIntent
	TGWPrepare       map[string]TGWOwner // pending attachments with no link yet
	TGWRoutes        []TGWRouteIntent
	TGWLive          map[string]struct{} // every record ID in the live buckets
	// SubnetAZs maps each local subnet to its zone.
	SubnetAZs     map[string]string
	TGWIncomplete error
}

// loadTransitGateways classifies every live Transit Gateway and attachment.
// vpcIDs comes from keys, so an unreadable VPC record never reads as absent.
func loadTransitGateways(ctx context.Context, js jetstream.JetStream, vpcs map[string]topology.VPCSpec, vpcIDs map[string]struct{}, subnetAZs map[string]string) TransitGatewayIntent {
	out := newTransitGatewayIntent(subnetAZs)
	if vpcIDs == nil {
		out.TGWIncomplete = errors.New("VPC bucket unavailable")
		return out
	}
	gws, atts, err := ec2projection.LoadTransitGateways(ctx, js)
	if err != nil {
		out.TGWIncomplete = err
		return out
	}
	classifyTransitGateways(&out, gws, atts, vpcs, vpcIDs)
	return out
}

// classifyTransitGateways fills out from the records. The result does not
// depend on the order of atts.
func classifyTransitGateways(out *TransitGatewayIntent, gws []ec2projection.TransitGateway, atts []ec2projection.TransitGatewayAttachment, vpcs map[string]topology.VPCSpec, vpcIDs map[string]struct{}) {
	subnetAZs := out.SubnetAZs
	for _, gw := range gws {
		out.TGWLive[gw.ID] = struct{}{}
		switch {
		case gw.CleanupPending:
			out.TGWTeardowns[gw.ID] = TGWOwner{gw.AccountID, gw.ID}
		case gw.Pending || gw.Available:
			out.TGWRouters[gw.ID] = TGWRouterIntent{AccountID: gw.AccountID, Generation: gw.Generation, Pending: gw.Pending}
		}
	}
	holders := map[string]ec2projection.TransitGatewayAttachment{}
	for _, a := range atts {
		out.TGWLive[a.ID] = struct{}{}
		vpc, local := vpcs[a.VpcID]
		_, here := vpcIDs[a.VpcID]
		// A VPC with no record has no known home, so any AZ may act for it.
		mine := local || !here
		linked := a.VPCAddr.Is4() && a.TGWAddr.Is4()
		switch {
		case a.Pending && !linked && mine:
			out.TGWPrepare[a.ID] = TGWOwner{a.AccountID, a.ID}
			continue
		case a.CleanupPending || !here:
			if mine {
				out.TGWLinkTeardowns[a.ID] = TGWLinkTeardownIntent{a.AccountID, a.TransitGatewayID, a.VpcID}
			}
			continue
		case !local || !linked || (!a.Pending && !a.Available):
			continue
		}
		if _, ok := out.TGWRouters[a.TransitGatewayID]; !ok {
			slog.Warn("reconcile/intent: attachment's Transit Gateway is not live",
				"transit_gateway_attachment_id", a.ID, "transit_gateway_id", a.TransitGatewayID)
			continue
		}
		azs := map[string]struct{}{}
		for _, id := range a.SubnetIDs {
			if az, ok := subnetAZs[id]; ok {
				azs[az] = struct{}{}
			}
		}
		out.TGWLinks[a.ID] = TGWLinkIntent{
			Spec: topology.TransitGatewayLinkSpec{
				AttachmentID: a.ID, TransitGatewayID: a.TransitGatewayID, VPCID: a.VpcID,
				VPCCIDR: vpc.CIDR, VPCAddr: a.VPCAddr, TGWAddr: a.TGWAddr,
			},
			AccountID: a.AccountID, Generation: a.Generation, Available: a.Available, AZs: azs,
		}
		key := a.TransitGatewayID + "|" + vpc.CIDR.String()
		if cur, ok := holders[key]; !ok || earlier(a, cur) {
			holders[key] = a
		}
	}
	for _, a := range holders {
		link := out.TGWLinks[a.ID]
		link.Holds = true
		out.TGWLinks[a.ID] = link
	}
}

func newTransitGatewayIntent(subnetAZs map[string]string) TransitGatewayIntent {
	return TransitGatewayIntent{
		TGWRouters:       map[string]TGWRouterIntent{},
		TGWTeardowns:     map[string]TGWOwner{},
		TGWLinks:         map[string]TGWLinkIntent{},
		TGWLinkTeardowns: map[string]TGWLinkTeardownIntent{},
		TGWPrepare:       map[string]TGWOwner{},
		TGWLive:          map[string]struct{}{},
		SubnetAZs:        subnetAZs,
	}
}

// earlier orders attachments by creation, then ID, so the holder of a range
// does not depend on read order.
func earlier(a, b ec2projection.TransitGatewayAttachment) bool {
	if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
		return c < 0
	}
	return a.ID < b.ID
}

// loadTGWRoutes fans every Transit Gateway route out over the subnets its table governs.
func loadTGWRoutes(
	vpcs map[string]topology.VPCSpec,
	subnets map[string]topology.SubnetSpec,
	routeTables []ec2routetable.RouteTableRecord,
) []TGWRouteIntent {
	targets := routeTableSubnets(subnets, routeTables)
	var out []TGWRouteIntent
	for _, rt := range routeTables {
		vpc, ok := vpcs[rt.VpcId]
		if !ok {
			continue
		}
		for _, r := range rt.Routes {
			if r.TransitGatewayId == "" {
				continue
			}
			dest, err := netip.ParsePrefix(r.DestinationCidrBlock)
			if err != nil {
				slog.Warn("reconcile/intent: Transit Gateway route CIDR parse failed",
					"route_table_id", rt.RouteTableId, "cidr", r.DestinationCidrBlock, "err", err)
				continue
			}
			exclude := gatewayExcludes(vpc.CIDR, dest, rt.Routes)
			for _, subnetID := range slices.Sorted(maps.Keys(targets[rt.RouteTableId])) {
				out = append(out, TGWRouteIntent{
					VPCID: rt.VpcId, RouteTableID: rt.RouteTableId, SubnetID: subnetID,
					Dest: dest, Exclude: exclude, TransitGatewayID: r.TransitGatewayId,
				})
			}
		}
	}
	slices.SortFunc(out, func(a, b TGWRouteIntent) int {
		return cmp.Or(cmp.Compare(a.SubnetID, b.SubnetID), a.Dest.Addr().Compare(b.Dest.Addr()), cmp.Compare(a.Dest.Bits(), b.Dest.Bits()))
	})
	return out
}

// gatewayExcludes is the VPC range and the table's IGW and NAT routes more
// specific than dest, which longest-prefix match sends elsewhere.
func gatewayExcludes(vpcCIDR, dest netip.Prefix, routes []ec2routetable.RouteRecord) []netip.Prefix {
	exclude := []netip.Prefix{vpcCIDR}
	for _, other := range routes {
		if !strings.HasPrefix(other.GatewayId, "igw-") && other.NatGatewayId == "" {
			continue
		}
		p, err := netip.ParsePrefix(other.DestinationCidrBlock)
		if err == nil && p.Bits() > dest.Bits() && dest.Contains(p.Addr()) {
			exclude = append(exclude, p)
		}
	}
	return exclude
}
