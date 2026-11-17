package reconcile

import (
	"context"
	"log/slog"
	"net/netip"
	"strings"

	ec2projection "github.com/mulgadc/spinifex/spinifex/domains/ec2/projection"
	ec2routetable "github.com/mulgadc/spinifex/spinifex/domains/ec2/routetable"
	"github.com/mulgadc/spinifex/spinifex/domains/network/topology"
	"github.com/nats-io/nats.go/jetstream"
)

// PeeringLinkIntent is a provisioning or active connection whose two VPCs
// exist and are local: its link and guards are realised here.
type PeeringLinkIntent struct {
	Spec       topology.PeeringLinkSpec
	Generation int64
	Active     bool
}

// PeeringTeardownIntent is a connection whose link must go: deleted with
// cleanup pending, or linked while one of its VPC records is absent.
type PeeringTeardownIntent struct {
	PcxID        string
	RequesterVPC string
	AccepterVPC  string
}

// PeeringRouteIntent is one route Dest → PcxID on one subnet its table governs.
// Exclude holds the VPC range and the table's more specific gateway routes in Dest.
type PeeringRouteIntent struct {
	VPCID        string
	RouteTableID string
	SubnetID     string
	Dest         netip.Prefix
	Exclude      []netip.Prefix
	PcxID        string
}

// PeeringIntent is the peering part of IntentState. Incomplete is set when
// the peering bucket or the VPC key set could not be read; the peering stages
// then change nothing, so a failed read never tears a link down.
type PeeringIntent struct {
	PeeringLinks      map[string]PeeringLinkIntent
	PeeringTeardowns  map[string]PeeringTeardownIntent
	PeeringRoutes     []PeeringRouteIntent
	PeeringLive       map[string]struct{}
	PeeringIncomplete error
}

// loadPeering classifies every live connection through EC2's projection.
// VPC presence comes from keys, so an unreadable VPC record never reads as absent.
func loadPeering(ctx context.Context, js jetstream.JetStream, localVPCs map[string]struct{}) PeeringIntent {
	out := PeeringIntent{
		PeeringLinks:     map[string]PeeringLinkIntent{},
		PeeringTeardowns: map[string]PeeringTeardownIntent{},
		PeeringLive:      map[string]struct{}{},
	}
	snap, err := ec2projection.LoadPeering(ctx, js)
	if err != nil {
		out.PeeringIncomplete = err
		return out
	}
	for _, c := range snap.Connections {
		out.PeeringLive[c.ID] = struct{}{}
		if !c.Linked && !c.CleanupPending {
			continue
		}
		_, reqHere := snap.VPCIDs[c.RequesterVPC]
		_, accHere := snap.VPCIDs[c.AccepterVPC]
		_, reqLocal := localVPCs[c.RequesterVPC]
		_, accLocal := localVPCs[c.AccepterVPC]
		if c.CleanupPending || !reqHere || !accHere {
			// A VPC with no record has no known home, so any AZ may clean it.
			if reqLocal || accLocal || (!reqHere && !accHere) {
				out.PeeringTeardowns[c.ID] = PeeringTeardownIntent{c.ID, c.RequesterVPC, c.AccepterVPC}
			}
			continue
		}
		if !reqLocal || !accLocal {
			continue
		}
		if !c.RequesterCIDR.IsValid() || !c.AccepterCIDR.IsValid() || !c.RequesterAddr.Is4() || !c.AccepterAddr.Is4() {
			slog.Warn("reconcile/intent: peering connection lacks a range or link address", "vpc_peering_connection_id", c.ID)
			continue
		}
		spec := topology.PeeringLinkSpec{
			PcxID: c.ID, RequesterVPC: c.RequesterVPC, AccepterVPC: c.AccepterVPC,
			RequesterCIDR: c.RequesterCIDR, AccepterCIDR: c.AccepterCIDR,
			RequesterAddr: c.RequesterAddr, AccepterAddr: c.AccepterAddr,
		}
		out.PeeringLinks[c.ID] = PeeringLinkIntent{Spec: spec, Generation: c.Generation, Active: c.Active}
	}
	return out
}

// loadPeeringRoutes fans every peering route out over the subnets its table governs.
func loadPeeringRoutes(
	vpcs map[string]topology.VPCSpec,
	subnets map[string]topology.SubnetSpec,
	routeTables []ec2routetable.RouteTableRecord,
) []PeeringRouteIntent {
	targets := routeTableSubnets(subnets, routeTables)
	var out []PeeringRouteIntent
	for _, rt := range routeTables {
		vpc, ok := vpcs[rt.VpcId]
		if !ok {
			continue
		}
		for _, r := range rt.Routes {
			if r.VpcPeeringConnectionId == "" {
				continue
			}
			dest, err := netip.ParsePrefix(r.DestinationCidrBlock)
			if err != nil {
				slog.Warn("reconcile/intent: peering route CIDR parse failed",
					"route_table_id", rt.RouteTableId, "cidr", r.DestinationCidrBlock, "err", err)
				continue
			}
			exclude := []netip.Prefix{vpc.CIDR}
			for _, other := range rt.Routes {
				if !strings.HasPrefix(other.GatewayId, "igw-") && other.NatGatewayId == "" {
					continue
				}
				p, err := netip.ParsePrefix(other.DestinationCidrBlock)
				if err == nil && p.Bits() > dest.Bits() && dest.Contains(p.Addr()) {
					exclude = append(exclude, p)
				}
			}
			for subnetID := range targets[rt.RouteTableId] {
				out = append(out, PeeringRouteIntent{
					VPCID: rt.VpcId, RouteTableID: rt.RouteTableId, SubnetID: subnetID,
					Dest: dest, Exclude: exclude, PcxID: r.VpcPeeringConnectionId,
				})
			}
		}
	}
	return out
}

// routeTableSubnets maps each table to the subnets it governs: its explicit
// associations, and for the main table every subnet with none.
func routeTableSubnets(subnets map[string]topology.SubnetSpec, routeTables []ec2routetable.RouteTableRecord) map[string]map[string]struct{} {
	explicit := map[string]struct{}{}
	for _, rt := range routeTables {
		for _, a := range rt.Associations {
			if a.SubnetId != "" && !a.Main {
				explicit[a.SubnetId] = struct{}{}
			}
		}
	}
	out := make(map[string]map[string]struct{}, len(routeTables))
	for _, rt := range routeTables {
		set := map[string]struct{}{}
		for _, a := range rt.Associations {
			if a.SubnetId != "" && !a.Main {
				set[a.SubnetId] = struct{}{}
			}
		}
		if rt.IsMain {
			for id, spec := range subnets {
				if _, ok := explicit[id]; !ok && spec.VPCID == rt.VpcId {
					set[id] = struct{}{}
				}
			}
		}
		out[rt.RouteTableId] = set
	}
	return out
}
