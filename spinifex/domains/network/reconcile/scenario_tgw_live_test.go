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

// tgwNB renders every Transit Gateway row on the three routers, UUIDs
// included, so an unchanged render proves a pass wrote nothing.
func tgwNB(t *testing.T, ctx context.Context, cli ovn.Client) string {
	t.Helper()
	var lines []string
	ports, err := cli.ListLogicalRouterPorts(ctx)
	if err != nil {
		t.Fatalf("ListLogicalRouterPorts: %v", err)
	}
	for _, p := range ports {
		if p.ExternalIDs[ovn.ExternalIDTGWAttachmentID] == "" {
			continue
		}
		peer := ""
		if p.Peer != nil {
			peer = *p.Peer
		}
		lines = append(lines, fmt.Sprintf("lrp %s %s %v peer=%s", p.UUID, p.Name, p.Networks, peer))
	}
	for _, router := range []string{"vpc-vpc-a", "vpc-vpc-b", topology.TransitGatewayRouter(testTGW)} {
		policies, err := cli.ListLogicalRouterPolicies(ctx, router)
		if err != nil {
			t.Fatalf("ListLogicalRouterPolicies %s: %v", router, err)
		}
		for _, p := range policies {
			if strings.HasPrefix(p.ExternalIDs[ovn.ExternalIDRole], "tgw") {
				lines = append(lines, fmt.Sprintf("lrpol %s %s %d %s %v %s", router, p.UUID, p.Priority, p.Action, p.Nexthops, p.Match))
			}
		}
		routes, err := cli.ListStaticRoutes(ctx, router)
		if err != nil {
			t.Fatalf("ListStaticRoutes %s: %v", router, err)
		}
		for _, r := range routes {
			if r.ExternalIDs[ovn.ExternalIDTGWAttachmentID] != "" {
				lines = append(lines, fmt.Sprintf("route %s %s %s via %s", router, r.UUID, r.IPPrefix, r.Nexthop))
			}
		}
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}

// Two attachments reconciled into a real NB link both VPCs to the Transit
// Gateway router, a steady pass writes nothing, drift is rewritten, and
// teardown leaves only the deleted attachment's route drop.
func TestScenario_TransitGateway_Live(t *testing.T) {
	rec, cli := newLiveReconciler(t)
	p := &tgwRecorder{}
	p.wire(rec)
	ctx := context.Background()

	if err := rec.Reconcile(ctx, tgwIntent(false)); err != nil {
		t.Fatalf("Reconcile pending: %v", err)
	}
	if err := rec.Reconcile(ctx, tgwIntent(true)); err != nil {
		t.Fatalf("Reconcile available: %v", err)
	}
	before := tgwNB(t, ctx, cli)
	for _, want := range []string{
		"tgwv-" + testAttA + " [169.254.64.5/30] peer=tgwt-" + testAttA,
		"tgwt-" + testAttB + " [169.254.64.10/30] peer=tgwv-" + testAttB,
		"10.0.0.0/16 via 169.254.64.5",
		"10.1.0.0/16 via 169.254.64.6",
		`1250 drop [] outport == "tgwv-` + testAttA + `"`,
		`1333 reroute [169.254.64.6] inport == "rtr-subnet-a" && ip4.dst == 10.1.0.0/16`,
		`1332 drop [] inport == "rtr-subnet-a2" && ip4.dst == 10.1.0.0/16`,
	} {
		if !strings.Contains(before, want) {
			t.Errorf("NB rows lack %q:\n%s", want, before)
		}
	}
	if strings.Contains(before, `reroute [169.254.64.6] inport == "rtr-subnet-a2"`) {
		t.Errorf("the subnet in a zone with no attachment subnet is rerouted:\n%s", before)
	}

	if err := rec.Reconcile(ctx, tgwIntent(true)); err != nil {
		t.Fatalf("Reconcile steady: %v", err)
	}
	if after := tgwNB(t, ctx, cli); after != before {
		t.Fatalf("steady pass changed NB:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}

	if err := cli.DeleteLogicalRouterPolicy(ctx, "vpc-vpc-a", 1250, `outport == "tgwv-`+testAttA+`"`); err != nil {
		t.Fatalf("DeleteLogicalRouterPolicy: %v", err)
	}
	if err := rec.Reconcile(ctx, tgwIntent(true)); err != nil {
		t.Fatalf("Reconcile drift: %v", err)
	}
	if got := tgwNB(t, ctx, cli); !strings.Contains(got, `1250 drop [] outport == "tgwv-`+testAttA+`"`) {
		t.Fatalf("a deleted guard is not rewritten:\n%s", got)
	}

	intent := tgwIntent(true)
	delete(intent.TGWLinks, testAttB)
	intent.TGWLinkTeardowns[testAttB] = TGWLinkTeardownIntent{testAcct, testTGW, "vpc-b"}
	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile teardown: %v", err)
	}
	gone := tgwNB(t, ctx, cli)
	if strings.Contains(gone, testAttB) || strings.Contains(gone, "10.1.0.0/16 via 169.254.64.9") {
		t.Errorf("rows of the deleted attachment remain:\n%s", gone)
	}
	if !strings.Contains(gone, `vpc-vpc-b`) || !strings.Contains(gone, `inport == "rtr-subnet-b" && ip4.dst == 10.0.0.0/16`) {
		t.Errorf("the route of the deleted attachment lost its drop:\n%s", gone)
	}
	lr, err := cli.GetLogicalRouter(ctx, topology.TransitGatewayRouter(testTGW))
	if err != nil {
		t.Fatalf("GetLogicalRouter: %v", err)
	}
	if len(lr.Ports) != 1 || len(lr.StaticRoutes) != 1 {
		t.Errorf("Transit Gateway router has %d ports and %d routes, want 1 and 1", len(lr.Ports), len(lr.StaticRoutes))
	}
}
