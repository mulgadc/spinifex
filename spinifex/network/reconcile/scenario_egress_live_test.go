package reconcile

// Egress scenarios extend the live-NB harness to IGW attachment and per-subnet
// egress steering (drop gates + public-instance exemptions). These exercise the
// Logical_Router_Policy path, which the core VPC/subnet/SG/ENI scenario does
// not touch, and re-assert idempotency over the policy-enriched NB snapshot.

import (
	"context"
	"net/netip"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

// TestScenario_IGWDropGatePublicInstance_Live attaches an IGW to a routeless
// subnet with a public-IP instance and asserts the reconciler installs both the
// drop gate and the /32 public-instance exemption above it, then that a second
// reconcile leaves the NB unchanged. This is the post-reboot regression shape
// from TestReconcile_PublicInstanceExemptFromDropGate, driven against real OVN.
func TestScenario_IGWDropGatePublicInstance_Live(t *testing.T) {
	rec, cli := newLiveReconciler(t)
	ctx := context.Background()

	intent := freshIntent(t)
	intent.IGWs["vpc-a"] = external.IGWSpec{VPCID: "vpc-a", InternetGatewayID: "igw-a"}
	port := intent.Ports["eni-a"]
	port.PublicIP = netip.MustParseAddr("192.168.0.50")
	intent.Ports["eni-a"] = port
	intent.DropGates[subnetEgressKey("subnet-a", netip.MustParsePrefix("0.0.0.0/0"))] = SubnetEgressIntent{
		VPCID: "vpc-a", SubnetID: "subnet-a", DestCIDR: netip.MustParsePrefix("0.0.0.0/0"),
	}

	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile #1: %v", err)
	}

	policies, err := cli.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-a"))
	if err != nil {
		t.Fatalf("ListLogicalRouterPolicies: %v", err)
	}
	var reroute, drop bool
	var reroutePrio, dropPrio int
	var rerouteMatch string
	for _, p := range policies {
		switch p.Priority {
		case policy.SystemInstanceEgressPriority:
			reroute, reroutePrio, rerouteMatch = true, p.Priority, p.Match
		case policy.SubnetEgressPriorityDrop:
			drop, dropPrio = true, p.Priority
		}
	}
	if !drop {
		t.Fatalf("drop gate (priority %d) missing on routeless IGW subnet", policy.SubnetEgressPriorityDrop)
	}
	if !reroute {
		t.Fatalf("public-instance exemption (priority %d) missing", policy.SystemInstanceEgressPriority)
	}
	if !strings.Contains(rerouteMatch, "ip4.src == 10.0.1.10/32") {
		t.Errorf("exemption match = %q, want ip4.src == 10.0.1.10/32", rerouteMatch)
	}
	if reroutePrio <= dropPrio {
		t.Errorf("exemption priority %d must sit above drop gate %d", reroutePrio, dropPrio)
	}

	before := snapshotNB(t, ctx, cli)
	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile #2: %v", err)
	}
	after := snapshotNB(t, ctx, cli)
	if before != after {
		t.Fatalf("reconcile not idempotent; NB changed on second pass:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}
