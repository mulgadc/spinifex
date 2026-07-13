package reconcile

import (
	"context"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

// newTestReconciler builds a reconciler around the in-memory OVN mock.
func newTestReconciler(t *testing.T) (*reconciler, *mock.Client) {
	t.Helper()
	m := mock.New()
	sg := policy.NewSecurityGroupManager(m)
	nat, err := policy.NewNATManager(m, policy.NATModeDistributed)
	if err != nil {
		t.Fatalf("NewNATManager: %v", err)
	}
	routes := policy.NewRouteManager(m)
	igw, err := external.NewIGWManager(external.IGWManagerConfig{
		OVN:       m,
		Routes:    routes,
		NAT:       nat,
		Allocator: external.LinkLocalAllocator{},
		NATMode:   policy.NATModeDistributed,
	})
	if err != nil {
		t.Fatalf("NewIGWManager: %v", err)
	}
	topo := topology.NewLiveManager(m)
	rec, err := New(Config{
		OVN: m, SG: sg, NAT: nat, Routes: routes, IGW: igw, Topology: topo,
		LocalAZ: "us-east-1a", NodeHostname: "test-host",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r, ok := rec.(*reconciler)
	if !ok {
		t.Fatalf("New returned %T, want *reconciler", rec)
	}
	return r, m
}

func TestReconcile_TopoOrder_VPCThenSubnetThenPort(t *testing.T) {
	rec, m := newTestReconciler(t)
	ctx := context.Background()

	intent := freshIntent(t)

	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if _, ok := m.Routers["vpc-"+intent.VPCs["vpc-a"].VPCID]; !ok {
		t.Errorf("VPC router not created")
	}
	if _, ok := m.Switches["subnet-"+intent.Subnets["subnet-a"].SubnetID]; !ok {
		t.Errorf("subnet switch not created")
	}
	if _, ok := m.Ports["port-"+intent.Ports["eni-a"].PortID]; !ok {
		t.Errorf("ENI port not created")
	}
	if _, ok := m.PortGroups[topology.SecurityGroupPortGroup("sg-a")]; !ok {
		t.Errorf("SG port group not created")
	}
}

func TestReconcile_PortJoinsPortGroupAtomically(t *testing.T) {
	rec, m := newTestReconciler(t)
	ctx := context.Background()

	intent := freshIntent(t)

	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	pg := m.PortGroups[topology.SecurityGroupPortGroup("sg-a")]
	if pg == nil {
		t.Fatal("port group not present")
	}
	port := m.Ports["port-"+intent.Ports["eni-a"].PortID]
	if port == nil {
		t.Fatal("ENI port not present")
	}
	if !slices.Contains(pg.Ports, port.UUID) {
		t.Errorf("ENI port not joined to SG port group atomically — racy gap revived")
	}
}

func TestReconcile_Idempotent(t *testing.T) {
	rec, m := newTestReconciler(t)
	ctx := context.Background()

	intent := freshIntent(t)

	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile #1: %v", err)
	}
	routerCountBefore := len(m.Routers)
	switchCountBefore := len(m.Switches)
	portCountBefore := len(m.Ports)
	pgCountBefore := len(m.PortGroups)
	aclUUIDsBefore := aclUUIDSet(m)

	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile #2: %v", err)
	}

	if len(m.Routers) != routerCountBefore {
		t.Errorf("second reconcile created duplicate routers: %d → %d", routerCountBefore, len(m.Routers))
	}
	if len(m.Switches) != switchCountBefore {
		t.Errorf("second reconcile created duplicate switches: %d → %d", switchCountBefore, len(m.Switches))
	}
	if len(m.Ports) != portCountBefore {
		t.Errorf("second reconcile created duplicate ports: %d → %d", portCountBefore, len(m.Ports))
	}
	if len(m.PortGroups) != pgCountBefore {
		t.Errorf("second reconcile created duplicate port groups: %d → %d", pgCountBefore, len(m.PortGroups))
	}
	// An unchanged SG must not churn ACL rows: ReplaceACLs no-ops, so every ACL
	// UUID from the first pass survives the second identical reconcile.
	aclUUIDsAfter := aclUUIDSet(m)
	if !maps.Equal(aclUUIDsBefore, aclUUIDsAfter) {
		t.Errorf("second reconcile churned ACL UUIDs: %v → %v", aclUUIDsBefore, aclUUIDsAfter)
	}
}

// aclUUIDSet snapshots the mock's current ACL UUIDs so idempotency can assert no
// churn across reconciles.
func aclUUIDSet(m *mock.Client) map[string]struct{} {
	m.Mu.Lock()
	defer m.Mu.Unlock()
	out := make(map[string]struct{}, len(m.ACLs))
	for uuid := range m.ACLs {
		out[uuid] = struct{}{}
	}
	return out
}

func TestReconcile_OrphanPortGroupRemoved(t *testing.T) {
	rec, m := newTestReconciler(t)
	ctx := context.Background()

	if err := m.CreatePortGroup(ctx, "sg_orphan", nil); err != nil {
		t.Fatalf("seed orphan port group: %v", err)
	}

	intent := IntentState{
		VPCs:    map[string]topology.VPCSpec{},
		Subnets: map[string]topology.SubnetSpec{},
		Ports:   map[string]topology.PortSpec{},
		SGs:     map[string]policy.SGSpec{},
		IGWs:    map[string]external.IGWSpec{},
		EIPs:    map[string]policy.EIPSpec{},
		NATGWs:  map[string]policy.NATGWSpec{},
	}

	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if _, ok := m.PortGroups["sg_orphan"]; ok {
		t.Errorf("orphan port group not removed")
	}
}

// ReconcileApplyOnly must not prune sg_* PGs on empty intent (startup race);
// full Reconcile must still prune.
func TestReconcile_ApplyOnlyKeepsOrphanPortGroup(t *testing.T) {
	rec, m := newTestReconciler(t)
	ctx := context.Background()

	if err := m.CreatePortGroup(ctx, "sg_orphan", nil); err != nil {
		t.Fatalf("seed orphan port group: %v", err)
	}

	intent := IntentState{
		VPCs:    map[string]topology.VPCSpec{},
		Subnets: map[string]topology.SubnetSpec{},
		Ports:   map[string]topology.PortSpec{},
		SGs:     map[string]policy.SGSpec{},
		IGWs:    map[string]external.IGWSpec{},
		EIPs:    map[string]policy.EIPSpec{},
		NATGWs:  map[string]policy.NATGWSpec{},
	}

	if err := rec.ReconcileApplyOnly(ctx, intent); err != nil {
		t.Fatalf("ReconcileApplyOnly: %v", err)
	}

	if _, ok := m.PortGroups["sg_orphan"]; !ok {
		t.Errorf("ReconcileApplyOnly pruned port group; startup race fix regressed")
	}

	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := m.PortGroups["sg_orphan"]; ok {
		t.Errorf("Reconcile failed to prune orphan after ApplyOnly path")
	}
}

// ReconcileApplyOnly must not prune orphan ENI LSPs on startup (in-flight ports
// before subscribers converge); full Reconcile must prune them.
func TestReconcile_ApplyOnlyKeepsOrphanLSP(t *testing.T) {
	rec, m := newTestReconciler(t)
	ctx := context.Background()

	intent := freshIntent(t)
	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	orphan := &nbdb.LogicalSwitchPort{
		Name: topology.Port("eni-orphan"),
		ExternalIDs: map[string]string{
			"spinifex:eni_id":    "eni-orphan",
			"spinifex:subnet_id": "subnet-a",
			"spinifex:vpc_id":    "vpc-a",
		},
	}
	if err := m.CreateLogicalSwitchPort(ctx, topology.SubnetSwitch("subnet-a"), orphan); err != nil {
		t.Fatalf("seed orphan LSP: %v", err)
	}

	if err := rec.ReconcileApplyOnly(ctx, intent); err != nil {
		t.Fatalf("ReconcileApplyOnly: %v", err)
	}
	if _, ok := m.Ports[topology.Port("eni-orphan")]; !ok {
		t.Errorf("ReconcileApplyOnly pruned orphan LSP; startup race fix regressed")
	}

	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := m.Ports[topology.Port("eni-orphan")]; ok {
		t.Errorf("Reconcile failed to prune orphan LSP after ApplyOnly path")
	}
}

func TestReconcile_ChassisRebindOnExistingIGW(t *testing.T) {
	m := mock.New()
	sg := policy.NewSecurityGroupManager(m)
	nat, _ := policy.NewNATManager(m, policy.NATModeDistributed)
	routes := policy.NewRouteManager(m)
	igw, _ := external.NewIGWManager(external.IGWManagerConfig{
		OVN: m, Routes: routes, NAT: nat,
		Allocator: external.LinkLocalAllocator{},
		NATMode:   policy.NATModeDistributed,
	})
	topo := topology.NewLiveManager(m)
	rec, err := New(Config{
		OVN: m, SG: sg, NAT: nat, Routes: routes, IGW: igw, Topology: topo,
		LocalAZ: "us-east-1a", NodeHostname: "test-host",
		Chassis: []string{"chassis-1", "chassis-2"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	// Seed VPC router first so AttachIGW can create the gateway LRP on it.
	seedIntent := IntentState{
		VPCs: map[string]topology.VPCSpec{
			"vpc-a": {VPCID: "vpc-a", CIDR: netip.MustParsePrefix("10.0.0.0/16"), VNI: 100},
		},
		Subnets: map[string]topology.SubnetSpec{},
		Ports:   map[string]topology.PortSpec{},
		SGs:     map[string]policy.SGSpec{},
		IGWs:    map[string]external.IGWSpec{},
		EIPs:    map[string]policy.EIPSpec{},
		NATGWs:  map[string]policy.NATGWSpec{},
	}
	if err := rec.Reconcile(ctx, seedIntent); err != nil {
		t.Fatalf("seed VPC reconcile: %v", err)
	}
	// Seed external switch + gateway LRP so apply takes the rebind branch.
	if err := igw.AttachIGW(ctx, external.IGWSpec{VPCID: "vpc-a", InternetGatewayID: "igw-a"}); err != nil {
		t.Fatalf("seed AttachIGW: %v", err)
	}
	setCallsBefore := m.SetGatewayChassisCalls

	intent := IntentState{
		VPCs:    map[string]topology.VPCSpec{},
		Subnets: map[string]topology.SubnetSpec{},
		Ports:   map[string]topology.PortSpec{},
		SGs:     map[string]policy.SGSpec{},
		IGWs:    map[string]external.IGWSpec{"vpc-a": {VPCID: "vpc-a", InternetGatewayID: "igw-a"}},
		EIPs:    map[string]policy.EIPSpec{},
		NATGWs:  map[string]policy.NATGWSpec{},
	}
	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if m.SetGatewayChassisCalls <= setCallsBefore {
		t.Errorf("expected SetGatewayChassis to fire on existing IGW for chassis rebind; calls before=%d after=%d",
			setCallsBefore, m.SetGatewayChassisCalls)
	}
}

// TestReconcile_GatewayClaimChecksChassisRedirectPort pins the verifier to the
// cr- Port_Binding. The LRP binding is chassis-less; checking it caused infinite
// recomputes and churned the EIP datapath.
func TestReconcile_GatewayClaimChecksChassisRedirectPort(t *testing.T) {
	withFastClaimBounds(t)
	m := mock.New()
	sg := policy.NewSecurityGroupManager(m)
	nat, _ := policy.NewNATManager(m, policy.NATModeDistributed)
	routes := policy.NewRouteManager(m)
	igw, _ := external.NewIGWManager(external.IGWManagerConfig{
		OVN: m, Routes: routes, NAT: nat,
		Allocator: external.LinkLocalAllocator{},
		NATMode:   policy.NATModeDistributed,
	})
	topo := topology.NewLiveManager(m)
	claim := &fakeClaimVerifier{claimedAfter: 0} // reports claimed immediately
	rec, err := New(Config{
		OVN: m, SG: sg, NAT: nat, Routes: routes, IGW: igw, Topology: topo,
		LocalAZ: "us-east-1a", NodeHostname: "test-host",
		Chassis: []string{"chassis-1"}, GatewayClaim: claim,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	intent := IntentState{
		VPCs: map[string]topology.VPCSpec{
			"vpc-a": {VPCID: "vpc-a", CIDR: netip.MustParsePrefix("10.0.0.0/16"), VNI: 100},
		},
		Subnets: map[string]topology.SubnetSpec{},
		Ports:   map[string]topology.PortSpec{},
		SGs:     map[string]policy.SGSpec{},
		IGWs:    map[string]external.IGWSpec{"vpc-a": {VPCID: "vpc-a", InternetGatewayID: "igw-a"}},
		EIPs:    map[string]policy.EIPSpec{},
		NATGWs:  map[string]policy.NATGWSpec{},
	}
	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if claim.checks == 0 {
		t.Fatal("gateway claim verifier never queried; rebind path did not run")
	}
	want := topology.GatewayChassisRedirectPort("vpc-a")
	if claim.lastPort != want {
		t.Errorf("claim verifier checked %q, want chassisredirect port %q (the LRP %q is always chassis-less)",
			claim.lastPort, want, topology.GatewayRouterPort("vpc-a"))
	}
	if claim.nudges != 0 {
		t.Errorf("claimed redirect port nudged %d recompute(s), want 0", claim.nudges)
	}
}

func TestReconcile_IGWAttachWhenTopologyMissing(t *testing.T) {
	m := mock.New()
	sg := policy.NewSecurityGroupManager(m)
	nat, _ := policy.NewNATManager(m, policy.NATModeDistributed)
	routes := policy.NewRouteManager(m)
	igw, _ := external.NewIGWManager(external.IGWManagerConfig{
		OVN: m, Routes: routes, NAT: nat,
		Allocator: external.LinkLocalAllocator{},
		NATMode:   policy.NATModeDistributed,
	})
	topo := topology.NewLiveManager(m)
	rec, err := New(Config{
		OVN: m, SG: sg, NAT: nat, Routes: routes, IGW: igw, Topology: topo,
		LocalAZ: "us-east-1a", NodeHostname: "test-host",
		Chassis: []string{"chassis-1"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	intent := IntentState{
		VPCs: map[string]topology.VPCSpec{
			"vpc-a": {VPCID: "vpc-a", CIDR: netip.MustParsePrefix("10.0.0.0/16"), VNI: 100},
		},
		Subnets: map[string]topology.SubnetSpec{},
		Ports:   map[string]topology.PortSpec{},
		SGs:     map[string]policy.SGSpec{},
		IGWs:    map[string]external.IGWSpec{"vpc-a": {VPCID: "vpc-a", InternetGatewayID: "igw-a"}},
		EIPs:    map[string]policy.EIPSpec{},
		NATGWs:  map[string]policy.NATGWSpec{},
	}

	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if _, ok := m.Switches[topology.ExternalSwitchShared()]; !ok {
		t.Errorf("shared external switch not created by reconciler AttachIGW path")
	}
	gwPort := topology.GatewayRouterPort("vpc-a")
	if _, ok := m.RouterPorts[gwPort]; !ok {
		t.Errorf("gateway LRP %s not created", gwPort)
	}

	switchCount := len(m.Switches)
	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile #2: %v", err)
	}
	if len(m.Switches) != switchCount {
		t.Errorf("second reconcile created duplicate switches: %d → %d", switchCount, len(m.Switches))
	}
}

func TestReconcile_PortMembershipDriftCorrected(t *testing.T) {
	rec, m := newTestReconciler(t)
	ctx := context.Background()

	intent := freshIntent(t)
	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile #1: %v", err)
	}

	intent.SGs["sg-b"] = policy.SGSpec{GroupID: "sg-b", VPCID: "vpc-a"}
	port := intent.Ports["eni-a"]
	port.SGIDs = append(port.SGIDs, "sg-b")
	intent.Ports["eni-a"] = port

	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile #2: %v", err)
	}

	pgB := m.PortGroups[topology.SecurityGroupPortGroup("sg-b")]
	if pgB == nil {
		t.Fatal("new SG port group not created")
	}
	storedPort := m.Ports["port-"+port.PortID]
	if !slices.Contains(pgB.Ports, storedPort.UUID) {
		t.Errorf("ENI port not joined to new SG port group on drift")
	}
}

// TestReconcile_PublicInstanceExemptFromDropGate locks the post-reboot regression: a
// reconcile that drop-gates an IGW-attached subnet with no 0.0.0.0/0 route must also
// install the /32 reroute above the gate for every public-IP instance in that subnet,
// else the gate drops the instance's inbound-connection reply and the ALB/EIP datapath
// goes dark post-reboot while every control-plane signal stays green. The reboot suite
// drives this via auto-assigned public IPs (ENI records), not the EIP bucket.
func TestReconcile_PublicInstanceExemptFromDropGate(t *testing.T) {
	ctx := context.Background()
	rec, m := newTestReconciler(t)

	intent := freshIntent(t)
	intent.IGWs["vpc-a"] = external.IGWSpec{VPCID: "vpc-a", InternetGatewayID: "igw-a"}
	// Auto-assigned public IP on the ENI (MapPublicIpOnLaunch / ELB), not an EIP.
	port := intent.Ports["eni-a"]
	port.PublicIP = netip.MustParseAddr("192.168.0.50")
	intent.Ports["eni-a"] = port
	intent.DropGates[subnetEgressKey("subnet-a", netip.MustParsePrefix("0.0.0.0/0"))] = SubnetEgressIntent{
		VPCID: "vpc-a", SubnetID: "subnet-a", DestCIDR: netip.MustParsePrefix("0.0.0.0/0"),
	}

	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-a"))
	if err != nil {
		t.Fatalf("ListLogicalRouterPolicies: %v", err)
	}
	var reroute, drop *nbdb.LogicalRouterPolicy
	for i := range policies {
		switch policies[i].Priority {
		case policy.SystemInstanceEgressPriority:
			reroute = &policies[i]
		case policy.SubnetEgressPriorityDrop:
			drop = &policies[i]
		}
	}
	if drop == nil {
		t.Fatalf("drop gate (priority %d) missing: a routeless IGW subnet must be gated", policy.SubnetEgressPriorityDrop)
	}
	if reroute == nil {
		t.Fatalf("public-instance exemption (priority %d) missing: the drop gate kills the reply post-reboot",
			policy.SystemInstanceEgressPriority)
	}
	if !strings.Contains(reroute.Match, "ip4.src == 10.0.1.10/32") {
		t.Errorf("exemption reroute match = %q, want it to confine to ip4.src == 10.0.1.10/32", reroute.Match)
	}
	if reroute.Priority <= drop.Priority {
		t.Errorf("exemption reroute priority %d must sit above drop gate %d", reroute.Priority, drop.Priority)
	}
}

// TestReconcile_PublicInstanceNoExemptionWithoutDropGate bounds the blast radius: a
// public-IP instance in a subnet with NO drop gate (routed subnet) must not get the
// /32 reroute — its priority-1000 subnet egress reroute already carries it, and an
// extra policy would needlessly override routed/NATGW egress.
func TestReconcile_PublicInstanceNoExemptionWithoutDropGate(t *testing.T) {
	ctx := context.Background()
	rec, m := newTestReconciler(t)

	intent := freshIntent(t)
	intent.IGWs["vpc-a"] = external.IGWSpec{VPCID: "vpc-a", InternetGatewayID: "igw-a"}
	port := intent.Ports["eni-a"]
	port.PublicIP = netip.MustParseAddr("192.168.0.50")
	intent.Ports["eni-a"] = port
	// No DropGates entry: the subnet is routed, not gated.

	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-a"))
	if err != nil {
		t.Fatalf("ListLogicalRouterPolicies: %v", err)
	}
	for _, p := range policies {
		if p.Priority == policy.SystemInstanceEgressPriority {
			t.Errorf("unexpected priority-%d reroute on an ungated subnet: %q", p.Priority, p.Match)
		}
	}
}

func TestDiffSets(t *testing.T) {
	add, remove := diffSets([]string{"a", "b", "c"}, []string{"b", "c", "d"})
	if !slices.Equal(sortedCopy(add), []string{"a"}) {
		t.Errorf("add = %v, want [a]", add)
	}
	if !slices.Equal(sortedCopy(remove), []string{"d"}) {
		t.Errorf("remove = %v, want [d]", remove)
	}
}

func freshIntent(t *testing.T) IntentState {
	t.Helper()
	mac, _ := net.ParseMAC("02:00:00:00:00:01")
	return IntentState{
		VPCs: map[string]topology.VPCSpec{
			"vpc-a": {VPCID: "vpc-a", CIDR: netip.MustParsePrefix("10.0.0.0/16"), VNI: 100},
		},
		Subnets: map[string]topology.SubnetSpec{
			"subnet-a": {SubnetID: "subnet-a", VPCID: "vpc-a", CIDR: netip.MustParsePrefix("10.0.1.0/24")},
		},
		Ports: map[string]topology.PortSpec{
			"eni-a": {PortID: "eni-a", SubnetID: "subnet-a", VPCID: "vpc-a",
				PrivateIP: netip.MustParseAddr("10.0.1.10"), MAC: mac, SGIDs: []string{"sg-a"}},
		},
		SGs: map[string]policy.SGSpec{
			"sg-a": {GroupID: "sg-a", VPCID: "vpc-a"},
		},
		IGWs:        map[string]external.IGWSpec{},
		EIPs:        map[string]policy.EIPSpec{},
		NATGWs:      map[string]policy.NATGWSpec{},
		IGWRoutes:   map[string]SubnetEgressIntent{},
		NATGWRoutes: map[string]SubnetEgressIntent{},
		DropGates:   map[string]SubnetEgressIntent{},
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	slices.Sort(out)
	return out
}

// TestFloatingIPSpecs covers the auto-assigned NAT reconcile gap: an ENI's
// auto-assigned public IP must be re-asserted as a dnat_and_snat (and run through
// guest-port convergence) alongside user EIPs, deduped when a user EIP already
// covers the same private IP.
func TestFloatingIPSpecs(t *testing.T) {
	mac, _ := net.ParseMAC("02:00:00:00:00:01")
	r := &reconciler{}

	intent := IntentState{
		EIPs: map[string]policy.EIPSpec{
			// User EIP on 172.31.0.9.
			"172.31.0.9": {VPCID: "vpc-a", ExternalIP: "192.168.1.200", LogicalIP: "172.31.0.9", PortName: topology.Port("eni-eip")},
			// User EIP that also has an auto-assigned public IP recorded on its
			// port: the EIP must win and the auto-assigned entry be skipped.
			"172.31.0.4": {VPCID: "vpc-a", ExternalIP: "192.168.1.201", LogicalIP: "172.31.0.4", PortName: topology.Port("eni-dup")},
		},
		Ports: map[string]topology.PortSpec{
			// Auto-assigned public IP with no user EIP -> must produce a spec.
			"eni-auto": {PortID: "eni-auto", VPCID: "vpc-b", PrivateIP: netip.MustParseAddr("172.31.0.5"),
				PublicIP: netip.MustParseAddr("192.168.1.116"), MAC: mac},
			// Same private IP as the user EIP above -> deduped out.
			"eni-dup": {PortID: "eni-dup", VPCID: "vpc-a", PrivateIP: netip.MustParseAddr("172.31.0.4"),
				PublicIP: netip.MustParseAddr("192.168.1.117"), MAC: mac},
			// No public IP -> skipped.
			"eni-nopub": {PortID: "eni-nopub", VPCID: "vpc-b", PrivateIP: netip.MustParseAddr("172.31.0.6")},
		},
	}

	specs := r.floatingIPSpecs(intent)

	byExternal := map[string]policy.EIPSpec{}
	for _, s := range specs {
		byExternal[s.ExternalIP] = s
	}

	// Both user EIPs present.
	if _, ok := byExternal["192.168.1.200"]; !ok {
		t.Errorf("user EIP 192.168.1.200 missing from specs")
	}
	if _, ok := byExternal["192.168.1.201"]; !ok {
		t.Errorf("user EIP 192.168.1.201 missing from specs")
	}
	// Auto-assigned public IP produced with the port's identity.
	auto, ok := byExternal["192.168.1.116"]
	if !ok {
		t.Fatalf("auto-assigned 192.168.1.116 missing from specs")
	}
	if auto.LogicalIP != "172.31.0.5" || auto.PortName != topology.Port("eni-auto") || auto.MAC != "02:00:00:00:00:01" {
		t.Errorf("auto-assigned spec malformed: %+v", auto)
	}
	// Deduped: the auto-assigned IP colliding with a user EIP's private IP is gone.
	if _, ok := byExternal["192.168.1.117"]; ok {
		t.Errorf("auto-assigned 192.168.1.117 should be deduped by the user EIP on 172.31.0.4")
	}
	// No public IP -> no spec for that port.
	if len(specs) != 3 {
		t.Errorf("want 3 specs (2 EIP + 1 auto), got %d: %+v", len(specs), specs)
	}
}
