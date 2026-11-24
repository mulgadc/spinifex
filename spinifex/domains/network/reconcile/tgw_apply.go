package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/netip"
	"slices"

	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/domains/network/policy"
	"github.com/mulgadc/spinifex/spinifex/domains/network/topology"
)

// TransitGatewayHooks report realization back to the Transit Gateway records.
// Each is optional: a nil hook changes no record, and a nil
// MarkAttachmentAvailable lets every link deliver.
type TransitGatewayHooks struct {
	// PrepareAttachment rewrites a pending attachment's parents and takes its
	// link, or fails it; the record change brings the next pass.
	PrepareAttachment           func(ctx context.Context, accountID, id string) error
	MarkTransitGatewayAvailable func(ctx context.Context, accountID, id string, generation int64) error
	MarkTransitGatewayCleaned   func(ctx context.Context, accountID, id string) error
	MarkAttachmentAvailable     func(ctx context.Context, accountID, id string, generation int64) error
	MarkAttachmentCleaned       func(ctx context.Context, accountID, id string) error
}

var defaultRoute = netip.MustParsePrefix("0.0.0.0/0")

// applyTransitGateways prepares new attachments, tears down deleted links and
// routers, writes each live router and link, and confirms pending ones. It
// returns the attachments that may deliver this pass.
func (r *reconciler) applyTransitGateways(ctx context.Context, intent IntentState, actual ActualState, res *passResult) map[string]bool {
	if intent.TGWIncomplete != nil {
		slog.Warn("reconcile/apply: Transit Gateway intent incomplete; left unchanged", "err", intent.TGWIncomplete)
		res.fail(classTGWLink, "intent", intent.TGWIncomplete)
		return nil
	}
	h := r.tgwHooks
	for _, id := range slices.Sorted(maps.Keys(intent.TGWPrepare)) {
		if h.PrepareAttachment == nil {
			break
		}
		if err := h.PrepareAttachment(ctx, intent.TGWPrepare[id].AccountID, id); err != nil {
			slog.Warn("reconcile/apply: preparing attachment failed", "transit_gateway_attachment_id", id, "err", err)
			res.fail(classTGWLink, id, err)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(intent.TGWLinkTeardowns)) {
		td := intent.TGWLinkTeardowns[id]
		routers := []string{topology.VPCRouter(td.VPCID), topology.TransitGatewayRouter(td.TransitGatewayID)}
		if err := r.ovn.DeleteTransitGatewayLink(ctx, id, routers); err != nil {
			slog.Error("reconcile/apply: attachment teardown failed", "transit_gateway_attachment_id", id, "err", err)
			res.fail(classTGWLink, id, err)
			continue
		}
		reportCleaned(ctx, h.MarkAttachmentCleaned, td.AccountID, id, classTGWLink, res)
	}
	for _, id := range slices.Sorted(maps.Keys(intent.TGWTeardowns)) {
		name := topology.TransitGatewayRouter(id)
		if _, ok := actual.Routers[name]; ok {
			if err := r.ovn.DeleteLogicalRouter(ctx, name); err != nil && !errors.Is(err, ovn.ErrLogicalRouterNotFound) {
				slog.Error("reconcile/apply: Transit Gateway router delete failed", "transit_gateway_id", id, "err", err)
				res.fail(classTGW, id, err)
				continue
			}
			delete(actual.Routers, name)
		}
		reportCleaned(ctx, h.MarkTransitGatewayCleaned, intent.TGWTeardowns[id].AccountID, id, classTGW, res)
	}
	for _, id := range slices.Sorted(maps.Keys(intent.TGWRouters)) {
		gw := intent.TGWRouters[id]
		name := topology.TransitGatewayRouter(id)
		if _, ok := actual.Routers[name]; !ok {
			lr := &nbdb.LogicalRouter{Name: name, ExternalIDs: map[string]string{ovn.ExternalIDTGWID: id}}
			if _, _, err := r.ovn.EnsureLogicalRouter(ctx, lr); err != nil {
				slog.Error("reconcile/apply: ensure Transit Gateway router failed", "transit_gateway_id", id, "err", err)
				res.fail(classTGW, id, err)
				continue
			}
			actual.Routers[name] = struct{}{}
			slog.Info("reconcile/apply: ensured Transit Gateway router", "transit_gateway_id", id)
		}
		if gw.Pending && h.MarkTransitGatewayAvailable != nil {
			if err := h.MarkTransitGatewayAvailable(ctx, gw.AccountID, id, gw.Generation); err != nil {
				slog.Warn("reconcile/apply: marking Transit Gateway available failed", "transit_gateway_id", id, "err", err)
				res.fail(classTGW, id, err)
			}
		}
	}
	return r.applyTGWLinks(ctx, intent, actual, res)
}

// reportCleaned calls a cleanup hook and records its failure for a retry next pass.
func reportCleaned(ctx context.Context, hook func(ctx context.Context, accountID, id string) error, accountID, id, class string, res *passResult) {
	if hook == nil {
		return
	}
	if err := hook(ctx, accountID, id); err != nil {
		slog.Warn("reconcile/apply: reporting cleanup failed", "id", id, "err", err)
		res.fail(class, id, err)
	}
}

func (r *reconciler) applyTGWLinks(ctx context.Context, intent IntentState, actual ActualState, res *passResult) map[string]bool {
	igw := map[string]bool{}
	for _, spec := range intent.IGWs {
		igw[spec.VPCID] = true
	}
	entries := map[string]map[netip.Prefix]struct{}{}
	for _, rt := range intent.TGWRoutes {
		if rt.Dest == defaultRoute && igw[rt.VPCID] {
			// The gateway's default route already carries it to the policy stage.
			continue
		}
		key := rt.VPCID + "|" + rt.TransitGatewayID
		if entries[key] == nil {
			entries[key] = map[netip.Prefix]struct{}{}
		}
		entries[key][rt.Dest] = struct{}{}
	}
	deliver := map[string]bool{}
	var pending []string
	for _, id := range slices.Sorted(maps.Keys(intent.TGWLinks)) {
		link := intent.TGWLinks[id]
		ends := tgwLinkEnds(link, entries[link.Spec.VPCID+"|"+link.Spec.TransitGatewayID])
		if missing := firstMissingRouter(actual, ends[0].Router, ends[1].Router); missing != "" {
			res.fail(classTGWLink, id, fmt.Errorf("logical router %s not present", missing))
			continue
		}
		if err := r.ovn.EnsureTransitGatewayLink(ctx, id, ends); err != nil {
			slog.Error("reconcile/apply: attachment link transaction failed", "transit_gateway_attachment_id", id, "err", err)
			res.fail(classTGWLink, id, err)
			continue
		}
		switch {
		case link.Available || r.tgwHooks.MarkAttachmentAvailable == nil:
			deliver[id] = true
		default:
			pending = append(pending, id)
		}
	}
	if len(pending) == 0 {
		return deliver
	}
	if r.flowsBarrier != nil {
		if err := r.flowsBarrier(); err != nil {
			slog.Warn("reconcile/apply: flows barrier failed before attachment activation", "err", err)
			for _, id := range pending {
				res.fail(classTGWLink, id, err)
			}
			return deliver
		}
	}
	for _, id := range pending {
		link := intent.TGWLinks[id]
		if err := r.tgwHooks.MarkAttachmentAvailable(ctx, link.AccountID, id, link.Generation); err != nil {
			slog.Warn("reconcile/apply: marking attachment available failed", "transit_gateway_attachment_id", id, "err", err)
			res.fail(classTGWLink, id, err)
			continue
		}
		deliver[id] = true
	}
	return deliver
}

func firstMissingRouter(actual ActualState, names ...string) string {
	for _, name := range names {
		if _, ok := actual.Routers[name]; !ok {
			return name
		}
	}
	return ""
}

// tgwLinkEnds builds both ends: the VPC router's port, guards and one entry
// route per destination, and the Transit Gateway's port and, if the link
// holds it, the route to the VPC's range.
func tgwLinkEnds(link TGWLinkIntent, dests map[netip.Prefix]struct{}) [2]ovn.TransitGatewayLinkEnd {
	spec := link.Spec
	vpc := ovn.TransitGatewayLinkEnd{
		Router:   topology.VPCRouter(spec.VPCID),
		Port:     topology.TransitGatewayVPCLinkPort(spec),
		Policies: []nbdb.LogicalRouterPolicy{policy.TransitGatewayGuard(spec), policy.TransitGatewayNoRoute(spec)},
	}
	for _, dest := range slices.SortedFunc(maps.Keys(dests), comparePrefix) {
		vpc.Routes = append(vpc.Routes, policy.TransitGatewayEntryRoute(spec, dest))
	}
	tgw := ovn.TransitGatewayLinkEnd{
		Router: topology.TransitGatewayRouter(spec.TransitGatewayID),
		Port:   topology.TransitGatewayLinkPort(spec),
	}
	if link.Holds {
		tgw.Routes = []nbdb.LogicalRouterStaticRoute{policy.TransitGatewayPropagation(spec)}
	}
	return [2]ovn.TransitGatewayLinkEnd{vpc, tgw}
}

func comparePrefix(a, b netip.Prefix) int {
	if c := a.Addr().Compare(b.Addr()); c != 0 {
		return c
	}
	return a.Bits() - b.Bits()
}

// applyTGWRoutes makes each local VPC router's Transit Gateway route rows
// exactly what its tables say. A route delivers only over a link in deliver,
// and only from a subnet in a zone the attachment has a subnet in.
func (r *reconciler) applyTGWRoutes(ctx context.Context, intent IntentState, actual ActualState, deliver map[string]bool, res *passResult) {
	if intent.TGWIncomplete != nil {
		return
	}
	links := map[string]string{}
	for id, link := range intent.TGWLinks {
		links[link.Spec.VPCID+"|"+link.Spec.TransitGatewayID] = id
	}
	want := map[string][]nbdb.LogicalRouterPolicy{}
	for _, route := range intent.TGWRoutes {
		spec := policy.TransitGatewayRouteSpec{
			RouteTableID: route.RouteTableID, SubnetID: route.SubnetID, Dest: route.Dest, Exclude: route.Exclude,
		}
		if id, ok := links[route.VPCID+"|"+route.TransitGatewayID]; ok && deliver[id] {
			link := intent.TGWLinks[id]
			if _, inZone := link.AZs[intent.SubnetAZs[route.SubnetID]]; inZone {
				spec.Reroute = &policy.TransitGatewayReroute{
					AttachmentID: id, Nexthop: link.Spec.TGWAddr, OutputPort: topology.TransitGatewayVPCPort(id),
				}
			}
		}
		want[route.VPCID] = append(want[route.VPCID], policy.TransitGatewayRouteRows(spec)...)
	}
	for _, vpcID := range slices.Sorted(maps.Keys(intent.VPCs)) {
		if _, ok := actual.Routers[topology.VPCRouter(vpcID)]; !ok {
			continue
		}
		err := r.peeringRoutes.SyncTransitGatewayRoutes(ctx, vpcID, want[vpcID])
		if errors.Is(err, ovn.ErrLogicalRouterNotFound) {
			slog.Debug("reconcile/apply: Transit Gateway route sync skipped, router gone", "vpc_id", vpcID, "err", err)
			continue
		}
		if err != nil {
			slog.Error("reconcile/apply: Transit Gateway route sync failed", "vpc_id", vpcID, "err", err)
			res.fail(classTGWRoute, vpcID, err)
		}
	}
}

// pruneTGWOrphans deletes Transit Gateway routers and attachment rows whose
// record is not in the live buckets. It is a backstop behind teardown and runs
// only on a complete read.
func (r *reconciler) pruneTGWOrphans(ctx context.Context, intent IntentState, res *passResult) {
	if intent.TGWIncomplete != nil {
		return
	}
	routers, err := r.ovn.ListLogicalRouters(ctx)
	if err != nil {
		res.fail(classTGWLink, "prune", err)
		return
	}
	ports, err := r.ovn.ListLogicalRouterPorts(ctx)
	if err != nil {
		res.fail(classTGWLink, "prune", err)
		return
	}
	orphans := map[string]struct{}{}
	for _, p := range ports {
		if id := p.ExternalIDs[ovn.ExternalIDTGWAttachmentID]; id != "" {
			orphans[id] = struct{}{}
		}
	}
	var names, orphanRouters []string
	for _, lr := range routers {
		names = append(names, lr.Name)
		if id := lr.ExternalIDs[ovn.ExternalIDTGWID]; id != "" {
			if _, live := intent.TGWLive[id]; !live {
				orphanRouters = append(orphanRouters, lr.Name)
			}
		}
		policies, err := r.ovn.ListLogicalRouterPolicies(ctx, lr.Name)
		if err != nil {
			res.fail(classTGWLink, "prune", err)
			return
		}
		for _, p := range policies {
			if id := p.ExternalIDs[ovn.ExternalIDTGWAttachmentID]; id != "" {
				orphans[id] = struct{}{}
			}
		}
		routes, err := r.ovn.ListStaticRoutes(ctx, lr.Name)
		if err != nil {
			res.fail(classTGWLink, "prune", err)
			return
		}
		for _, rt := range routes {
			if id := rt.ExternalIDs[ovn.ExternalIDTGWAttachmentID]; id != "" {
				orphans[id] = struct{}{}
			}
		}
	}
	for id := range intent.TGWLive {
		delete(orphans, id)
	}
	for _, id := range slices.Sorted(maps.Keys(orphans)) {
		if err := r.ovn.DeleteTransitGatewayLink(ctx, id, names); err != nil {
			res.fail(classTGWLink, id, err)
			continue
		}
		slog.Info("reconcile/apply: removed orphan attachment rows", "transit_gateway_attachment_id", id)
	}
	for _, name := range orphanRouters {
		if err := r.ovn.DeleteLogicalRouter(ctx, name); err != nil && !errors.Is(err, ovn.ErrLogicalRouterNotFound) {
			res.fail(classTGW, name, err)
			continue
		}
		slog.Info("reconcile/apply: removed orphan Transit Gateway router", "router", name)
	}
}
