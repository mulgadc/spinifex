package reconcile

// Kitchen-sink golden scenario. It reconciles an intent that exercises every
// resource type the reconciler wires — VPC, two subnets, two ENIs, an SG with
// real tenant rules, IGW attach, associated EIP, NAT gateway, IGW/NATGW egress
// reroutes and a drop gate — into a real in-process NB, then asserts the whole
// normalized NB snapshot matches a checked-in golden. This mechanically enforces
// failure-mode 2: adding a resource type without wiring it (or wiring it wrong)
// diffs the golden and fails. Regenerate with UPDATE_GOLDEN=1 go test.

import (
	"context"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

// kitchenSinkIntent builds a maximal IntentState touching every applyX path:
// a public subnet (IGW egress) and a private subnet (NATGW egress) under one VPC.
func kitchenSinkIntent(t *testing.T) IntentState {
	t.Helper()
	macA, _ := net.ParseMAC("02:00:00:00:00:01")
	macB, _ := net.ParseMAC("02:00:00:00:00:02")
	defaultRoute := netip.MustParsePrefix("0.0.0.0/0")
	pubCIDR := netip.MustParsePrefix("10.0.1.0/24")
	privCIDR := netip.MustParsePrefix("10.0.2.0/24")

	return IntentState{
		VPCs: map[string]topology.VPCSpec{
			"vpc-a": {VPCID: "vpc-a", CIDR: netip.MustParsePrefix("10.0.0.0/16"), VNI: 100},
		},
		Subnets: map[string]topology.SubnetSpec{
			"subnet-pub":  {SubnetID: "subnet-pub", VPCID: "vpc-a", CIDR: pubCIDR},
			"subnet-priv": {SubnetID: "subnet-priv", VPCID: "vpc-a", CIDR: privCIDR},
		},
		Ports: map[string]topology.PortSpec{
			"eni-pub": {PortID: "eni-pub", SubnetID: "subnet-pub", VPCID: "vpc-a",
				PrivateIP: netip.MustParseAddr("10.0.1.10"), MAC: macA, SGIDs: []string{"sg-a"},
				PublicIP: netip.MustParseAddr("192.168.0.50")},
			"eni-priv": {PortID: "eni-priv", SubnetID: "subnet-priv", VPCID: "vpc-a",
				PrivateIP: netip.MustParseAddr("10.0.2.10"), MAC: macB, SGIDs: []string{"sg-a"}},
		},
		SGs: map[string]policy.SGSpec{
			"sg-a": {GroupID: "sg-a", VPCID: "vpc-a",
				IngressRules: []policy.Rule{
					{IPProtocol: "tcp", FromPort: 22, ToPort: 22, CIDR: "0.0.0.0/0"},
					{IPProtocol: "tcp", FromPort: 443, ToPort: 443, CIDR: "0.0.0.0/0"},
				},
				EgressRules: []policy.Rule{
					{IPProtocol: "-1", FromPort: 0, ToPort: 0, CIDR: "0.0.0.0/0"},
				}},
		},
		IGWs: map[string]external.IGWSpec{
			"vpc-a": {VPCID: "vpc-a", InternetGatewayID: "igw-a"},
		},
		EIPs: map[string]policy.EIPSpec{
			"10.0.1.10": {VPCID: "vpc-a", ExternalIP: "192.168.0.50", LogicalIP: "10.0.1.10",
				PortName: topology.Port("eni-pub"), MAC: "02:00:00:00:00:01"},
		},
		NATGWs: map[string]policy.NATGWSpec{
			natgwSpecKey("nat-a", privCIDR.String()): {VPCID: "vpc-a", NATGatewayID: "nat-a",
				PublicIP: "192.168.0.60", SubnetCIDR: privCIDR.String()},
		},
		IGWRoutes: map[string]SubnetEgressIntent{
			subnetEgressKey("subnet-pub", defaultRoute): {VPCID: "vpc-a", SubnetID: "subnet-pub", DestCIDR: defaultRoute},
		},
		NATGWRoutes: map[string]SubnetEgressIntent{
			subnetEgressKey("subnet-priv", defaultRoute): {VPCID: "vpc-a", SubnetID: "subnet-priv", DestCIDR: defaultRoute},
		},
		DropGates: map[string]SubnetEgressIntent{
			subnetEgressKey("subnet-pub", defaultRoute):  {VPCID: "vpc-a", SubnetID: "subnet-pub", DestCIDR: defaultRoute},
			subnetEgressKey("subnet-priv", defaultRoute): {VPCID: "vpc-a", SubnetID: "subnet-priv", DestCIDR: defaultRoute},
		},
	}
}

func TestScenario_KitchenSinkGolden_Live(t *testing.T) {
	rec, cli := newLiveReconciler(t)
	ctx := context.Background()
	intent := kitchenSinkIntent(t)

	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile #1: %v", err)
	}

	got := snapshotNB(t, ctx, cli)

	goldenPath := filepath.Join("testdata", "kitchen_sink_nb.golden")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("golden updated: %s", goldenPath)
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (regenerate with UPDATE_GOLDEN=1): %v", err)
	}
	if got != string(want) {
		t.Fatalf("NB snapshot differs from golden; a resource type may be unwired or mis-wired.\n"+
			"Run UPDATE_GOLDEN=1 go test to accept intended changes.\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}

	// Idempotency over the full kitchen-sink state.
	before := snapshotNB(t, ctx, cli)
	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile #2: %v", err)
	}
	after := snapshotNB(t, ctx, cli)
	if before != after {
		t.Fatalf("reconcile not idempotent; NB changed on second pass:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}
