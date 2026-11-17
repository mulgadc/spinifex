package reconcile

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"

	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/domains/network/policy"
	"github.com/mulgadc/spinifex/spinifex/domains/network/topology"
)

// applyPeeringLinks tears down every connection in PeeringTeardowns, then
// writes each link and its guards as one transaction and confirms a
// provisioning connection active. It returns the links that may deliver this
// pass: realised, and active before the pass or confirmed active during it.
func (r *reconciler) applyPeeringLinks(ctx context.Context, intent IntentState, actual ActualState, res *passResult) map[string]bool {
	if intent.PeeringIncomplete != nil {
		slog.Warn("reconcile/apply: peering intent incomplete; peering left unchanged", "err", intent.PeeringIncomplete)
		res.fail(classPeeringLink, "intent", intent.PeeringIncomplete)
		return nil
	}
	for _, id := range slices.Sorted(maps.Keys(intent.PeeringTeardowns)) {
		td := intent.PeeringTeardowns[id]
		routers := []string{topology.VPCRouter(td.RequesterVPC), topology.VPCRouter(td.AccepterVPC)}
		if err := r.ovn.DeletePeeringLink(ctx, id, routers); err != nil {
			slog.Error("reconcile/apply: peering teardown failed", "vpc_peering_connection_id", id, "err", err)
			res.fail(classPeeringLink, id, err)
			continue
		}
		if r.markPeeringCleaned == nil {
			continue
		}
		if err := r.markPeeringCleaned(ctx, id); err != nil {
			slog.Warn("reconcile/apply: marking peering connection cleaned failed", "vpc_peering_connection_id", id, "err", err)
			res.fail(classPeeringLink, id, err)
		}
	}

	deliver := map[string]bool{}
	var pending []string
	for _, id := range slices.Sorted(maps.Keys(intent.PeeringLinks)) {
		link := intent.PeeringLinks[id]
		ends := peeringLinkEnds(link.Spec)
		if missing := missingRouter(ends, actual); missing != "" {
			res.fail(classPeeringLink, id, fmt.Errorf("logical router %s not present", missing))
			continue
		}
		if !r.peeringLinkConverged(ctx, ends) {
			if err := r.ovn.EnsurePeeringLink(ctx, ends); err != nil {
				slog.Error("reconcile/apply: peering link transaction failed", "vpc_peering_connection_id", id, "err", err)
				res.fail(classPeeringLink, id, err)
				continue
			}
			slog.Info("reconcile/apply: peering link written", "vpc_peering_connection_id", id)
		}
		switch {
		case link.Active || r.markPeeringActive == nil:
			deliver[id] = true
		default:
			pending = append(pending, id)
		}
	}
	for _, id := range r.confirmPeeringActive(ctx, intent.PeeringLinks, pending, res) {
		deliver[id] = true
	}
	return deliver
}

// confirmPeeringActive waits once for chassis to install the links' flows,
// then reports each generation realised and returns those it confirmed. A
// failed report is retried next pass.
func (r *reconciler) confirmPeeringActive(ctx context.Context, links map[string]PeeringLinkIntent, ids []string, res *passResult) []string {
	if len(ids) == 0 {
		return nil
	}
	if r.flowsBarrier != nil {
		if err := r.flowsBarrier(); err != nil {
			slog.Warn("reconcile/apply: flows barrier failed before peering activation", "err", err)
			for _, id := range ids {
				res.fail(classPeeringLink, id, err)
			}
			return nil
		}
	}
	var confirmed []string
	for _, id := range ids {
		if err := r.markPeeringActive(ctx, id, links[id].Generation); err != nil {
			slog.Warn("reconcile/apply: marking peering connection active failed", "vpc_peering_connection_id", id, "err", err)
			res.fail(classPeeringLink, id, err)
			continue
		}
		confirmed = append(confirmed, id)
	}
	return confirmed
}

// missingRouter returns the first end whose router is not in actual, or "".
func missingRouter(ends [2]ovn.PeeringLinkEnd, actual ActualState) string {
	for _, end := range ends {
		if _, ok := actual.Routers[end.Router]; !ok {
			return end.Router
		}
	}
	return ""
}

func peeringLinkEnds(spec topology.PeeringLinkSpec) [2]ovn.PeeringLinkEnd {
	var ends [2]ovn.PeeringLinkEnd
	for i, side := range spec.Sides() {
		ends[i] = ovn.PeeringLinkEnd{
			Router:  topology.VPCRouter(side.LocalVPC),
			Port:    topology.PeeringLinkPort(side),
			Guard:   policy.PeeringGuard(side),
			Entry:   policy.PeeringEntryRoute(side),
			NoRoute: policy.PeeringNoRouteDrop(side),
		}
	}
	return ends
}

// peeringLinkConverged reports whether every row of both ends already
// matches, exactly once, so a steady-state pass writes nothing.
func (r *reconciler) peeringLinkConverged(ctx context.Context, ends [2]ovn.PeeringLinkEnd) bool {
	for _, end := range ends {
		port, err := r.ovn.GetLogicalRouterPort(ctx, end.Port.Name)
		if err != nil || !peeringPortMatches(port, &end.Port) {
			return false
		}
		policies, err := r.ovn.ListLogicalRouterPolicies(ctx, end.Router)
		if err != nil || !peeringPolicyConverged(policies, &end.Guard) || !peeringPolicyConverged(policies, &end.NoRoute) {
			return false
		}
		routes, err := r.ovn.ListStaticRoutes(ctx, end.Router)
		if err != nil || !peeringEntryConverged(routes, &end.Entry) {
			return false
		}
	}
	return true
}

// peeringPolicyConverged reports one policy of want's connection and role, equal to want.
func peeringPolicyConverged(policies []nbdb.LogicalRouterPolicy, want *nbdb.LogicalRouterPolicy) bool {
	found := 0
	for i := range policies {
		p := &policies[i]
		if !ovn.SamePeeringOwner(p.ExternalIDs, want.ExternalIDs) {
			continue
		}
		if found++; p.Priority != want.Priority || p.Match != want.Match || p.Action != want.Action {
			return false
		}
	}
	return found == 1
}

// peeringEntryConverged reports one static route of want's connection, equal to want.
func peeringEntryConverged(routes []nbdb.LogicalRouterStaticRoute, want *nbdb.LogicalRouterStaticRoute) bool {
	found := 0
	for i := range routes {
		rt := &routes[i]
		if !ovn.SamePeeringOwner(rt.ExternalIDs, want.ExternalIDs) {
			continue
		}
		if found++; rt.IPPrefix != want.IPPrefix || rt.Nexthop != want.Nexthop ||
			rt.OutputPort == nil || *rt.OutputPort != *want.OutputPort {
			return false
		}
	}
	return found == 1
}

func peeringPortMatches(cur, want *nbdb.LogicalRouterPort) bool {
	return cur.MAC == want.MAC && slices.Equal(cur.Networks, want.Networks) &&
		cur.Peer != nil && want.Peer != nil && *cur.Peer == *want.Peer &&
		cur.ExternalIDs[ovn.ExternalIDPcxID] == want.ExternalIDs[ovn.ExternalIDPcxID]
}

// applyPeeringRoutes makes each local VPC router's peering route policies
// exactly what its tables say. A route delivers only over a link in deliver;
// every other route keeps only its drop.
func (r *reconciler) applyPeeringRoutes(ctx context.Context, intent IntentState, actual ActualState, deliver map[string]bool, res *passResult) {
	if intent.PeeringIncomplete != nil {
		return
	}
	want := map[string][]nbdb.LogicalRouterPolicy{}
	for _, route := range intent.PeeringRoutes {
		spec := policy.PeeringRouteSpec{
			RouteTableID: route.RouteTableID, SubnetID: route.SubnetID, Dest: route.Dest, Exclude: route.Exclude,
		}
		if link, ok := intent.PeeringLinks[route.PcxID]; ok && deliver[route.PcxID] {
			if side, ok := link.Spec.Side(route.VPCID); ok {
				spec.Reroute = &policy.PeeringReroute{PcxID: route.PcxID, Nexthop: side.PeerAddr, OutputPort: side.LocalPort, PeerCIDR: side.PeerCIDR}
			}
		}
		want[route.VPCID] = append(want[route.VPCID], policy.PeeringRouteRows(spec)...)
	}
	for _, vpcID := range slices.Sorted(maps.Keys(intent.VPCs)) {
		if _, ok := actual.Routers[topology.VPCRouter(vpcID)]; !ok {
			continue
		}
		if err := r.peeringRoutes.SyncPeeringRoutes(ctx, vpcID, want[vpcID]); err != nil {
			slog.Error("reconcile/apply: peering route sync failed", "vpc_id", vpcID, "err", err)
			res.fail(classPeeringRoute, vpcID, err)
		}
	}
}

// prunePeeringOrphans deletes link ports, policies and entry routes whose connection
// has no live record. It is a backstop behind teardown and runs only on a
// complete read of the peering bucket.
func (r *reconciler) prunePeeringOrphans(ctx context.Context, intent IntentState, res *passResult) {
	if intent.PeeringIncomplete != nil {
		return
	}
	ports, err := r.ovn.ListLogicalRouterPorts(ctx)
	if err != nil {
		res.fail(classPeeringLink, "prune", err)
		return
	}
	routers, err := r.ovn.ListLogicalRouters(ctx)
	if err != nil {
		res.fail(classPeeringLink, "prune", err)
		return
	}
	orphans := map[string]struct{}{}
	for _, p := range ports {
		if id := p.ExternalIDs[ovn.ExternalIDPcxID]; id != "" {
			orphans[id] = struct{}{}
		}
	}
	names := make([]string, 0, len(routers))
	for _, lr := range routers {
		names = append(names, lr.Name)
		policies, err := r.ovn.ListLogicalRouterPolicies(ctx, lr.Name)
		if err != nil {
			res.fail(classPeeringLink, "prune", err)
			return
		}
		for _, p := range policies {
			if id := p.ExternalIDs[ovn.ExternalIDPcxID]; id != "" {
				orphans[id] = struct{}{}
			}
		}
		routes, err := r.ovn.ListStaticRoutes(ctx, lr.Name)
		if err != nil {
			res.fail(classPeeringLink, "prune", err)
			return
		}
		for _, rt := range routes {
			if id := rt.ExternalIDs[ovn.ExternalIDPcxID]; id != "" {
				orphans[id] = struct{}{}
			}
		}
	}
	for id := range intent.PeeringLive {
		delete(orphans, id)
	}
	for _, id := range slices.Sorted(maps.Keys(orphans)) {
		if err := r.ovn.DeletePeeringLink(ctx, id, names); err != nil {
			res.fail(classPeeringLink, id, err)
			continue
		}
		slog.Info("reconcile/apply: removed orphan peering rows", "vpc_peering_connection_id", id)
	}
}
