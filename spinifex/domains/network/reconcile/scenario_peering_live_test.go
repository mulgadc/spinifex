package reconcile

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/domains/network/topology"
)

// peeringNB renders every peering-owned port and policy on the two routers,
// UUIDs included, so an unchanged render proves a pass wrote nothing.
func peeringNB(t *testing.T, ctx context.Context, cli ovn.Client) string {
	t.Helper()
	var lines []string
	ports, err := cli.ListLogicalRouterPorts(ctx)
	if err != nil {
		t.Fatalf("ListLogicalRouterPorts: %v", err)
	}
	for _, p := range ports {
		if p.ExternalIDs[ovn.ExternalIDPcxID] == "" {
			continue
		}
		peer := ""
		if p.Peer != nil {
			peer = *p.Peer
		}
		lines = append(lines, fmt.Sprintf("lrp %s %s %s %v peer=%s", p.UUID, p.Name, p.MAC, p.Networks, peer))
	}
	for _, router := range []string{"vpc-vpc-a", "vpc-vpc-b"} {
		policies, err := cli.ListLogicalRouterPolicies(ctx, router)
		if err != nil {
			t.Fatalf("ListLogicalRouterPolicies %s: %v", router, err)
		}
		for _, p := range policies {
			if !strings.HasPrefix(p.ExternalIDs[ovn.ExternalIDRole], "peering") {
				continue
			}
			nh := ""
			if p.Nexthop != nil {
				nh = *p.Nexthop
			}
			lines = append(lines, fmt.Sprintf("lrpol %s %s %d %s %s %s", router, p.UUID, p.Priority, p.Action, nh, p.Match))
		}
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}

// A peering connection reconciled into a real NB joins the two routers, a
// steady-state pass writes nothing, and teardown leaves only the route drops.
func TestScenario_PeeringLink_Live(t *testing.T) {
	rec, cli := newLiveReconciler(t)
	p := &peeringRecorder{}
	p.wire(rec)
	ctx := context.Background()

	if err := rec.Reconcile(ctx, peeringIntent(false)); err != nil {
		t.Fatalf("Reconcile provisioning: %v", err)
	}
	if err := rec.Reconcile(ctx, peeringIntent(true)); err != nil {
		t.Fatalf("Reconcile active: %v", err)
	}
	reqPort, err := cli.GetLogicalRouterPort(ctx, topology.PeeringRouterPort(testPcx, "vpc-a"))
	if err != nil {
		t.Fatalf("requester link port: %v", err)
	}
	if reqPort.Peer == nil || *reqPort.Peer != topology.PeeringRouterPort(testPcx, "vpc-b") {
		t.Errorf("requester peer = %v", reqPort.Peer)
	}
	lr, err := cli.GetLogicalRouter(ctx, "vpc-vpc-a")
	if err != nil {
		t.Fatalf("GetLogicalRouter: %v", err)
	}
	if !slices.Contains(lr.Ports, reqPort.UUID) {
		t.Error("requester link port is not on its router")
	}
	before := peeringNB(t, ctx, cli)
	for _, want := range []string{"drop", "reroute", "169.254.192.2"} {
		if !strings.Contains(before, want) {
			t.Errorf("NB peering rows lack %q:\n%s", want, before)
		}
	}

	if err := rec.Reconcile(ctx, peeringIntent(true)); err != nil {
		t.Fatalf("Reconcile steady: %v", err)
	}
	if after := peeringNB(t, ctx, cli); after != before {
		t.Fatalf("steady-state pass changed NB:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}

	intent := peeringIntent(true)
	intent.PeeringLinks = map[string]PeeringLinkIntent{}
	intent.PeeringTeardowns = map[string]PeeringTeardownIntent{testPcx: {testPcx, "vpc-a", "vpc-b"}}
	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile teardown: %v", err)
	}
	gone := peeringNB(t, ctx, cli)
	if strings.Contains(gone, "lrp ") || strings.Contains(gone, "reroute") || strings.Count(gone, "lrpol ") != 2 {
		t.Errorf("after teardown want only the two route drops:\n%s", gone)
	}
	lr, err = cli.GetLogicalRouter(ctx, "vpc-vpc-a")
	if err != nil {
		t.Fatalf("GetLogicalRouter: %v", err)
	}
	if slices.Contains(lr.Ports, reqPort.UUID) {
		t.Error("router still references the deleted link port")
	}
}
