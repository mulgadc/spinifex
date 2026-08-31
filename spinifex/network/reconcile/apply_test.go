package reconcile

import (
	"context"
	"errors"
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

// TestReconcile_PortSuppressDHCP proves applyPorts' create branch never sets
// DHCPv4Options for a SuppressDHCP port (a statically-addressed customer
// ENI), while an ordinary port on the same subnet still gets one — this is
// the drift/recreate path, distinct from EnsurePort's, that must not re-add
// a lease the guest's dhcpcd would flush the static address on.
func TestReconcile_PortSuppressDHCP(t *testing.T) {
	rec, m := newTestReconciler(t)
	ctx := context.Background()

	intent := freshIntent(t)
	mac, _ := net.ParseMAC("02:00:00:00:00:09")
	intent.Ports["eni-static"] = topology.PortSpec{
		PortID: "eni-static", SubnetID: "subnet-a", VPCID: "vpc-a",
		PrivateIP: netip.MustParseAddr("10.0.1.11"), MAC: mac,
		SGIDs: []string{"sg-a"}, SuppressDHCP: true,
	}

	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	dhcpLSP := m.Ports["port-"+intent.Ports["eni-a"].PortID]
	if dhcpLSP == nil {
		t.Fatal("ordinary ENI port not created")
	}
	if dhcpLSP.DHCPv4Options == nil {
		t.Errorf("ordinary ENI's LSP must carry dhcpv4_options")
	}

	staticLSP := m.Ports["port-eni-static"]
	if staticLSP == nil {
		t.Fatal("static ENI port not created")
	}
	if staticLSP.DHCPv4Options != nil {
		t.Errorf("SuppressDHCP ENI's LSP must not carry dhcpv4_options, got %q", *staticLSP.DHCPv4Options)
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

// Every SG ACL, including the priority 900/800 default-denies, hangs off a port
// group, so a port in none of them is unrestricted. When a port's policy cannot
// be programmed in full, applyPorts must refuse the port rather than create it.
func TestReconcile_UnprogrammablePolicyDoesNotCreatePort(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name   string
		mutate func(*IntentState)
	}{
		{
			// The OVN-recreate / cold-chassis window: applySGs has not yet
			// built the port group this pass.
			name: "every port group missing",
			mutate: func(in *IntentState) {
				delete(in.SGs, "sg-a")
			},
		},
		{
			// A partial miss must not land the port in the subset that exists:
			// the absent SG's rules would silently not apply.
			name: "one port group missing",
			mutate: func(in *IntentState) {
				port := in.Ports["eni-a"]
				port.SGIDs = append(port.SGIDs, "sg-b")
				in.Ports["eni-a"] = port
			},
		},
		{
			// A legacy ENI record written before SG defaulting: the field is
			// omitempty and unmarshals to nil with no migration to backfill it.
			name: "no security groups at all",
			mutate: func(in *IntentState) {
				port := in.Ports["eni-a"]
				port.SGIDs = nil
				in.Ports["eni-a"] = port
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec, m := newTestReconciler(t)
			intent := freshIntent(t)
			tc.mutate(&intent)

			err := rec.Reconcile(ctx, intent)
			if !errors.Is(err, ErrPassIncomplete) {
				t.Errorf("Reconcile err = %v, want ErrPassIncomplete: the port must be reported unconverged", err)
			}
			if _, ok := m.Ports["port-eni-a"]; ok {
				t.Error("port created with an unprogrammable policy — it would be unrestricted")
			}
		})
	}
}

// The same rule on the update path: an existing port whose desired groups have
// gone missing keeps the memberships it has. Applying the diff would strip a
// live guest bare, which is worse than leaving it on a stale policy.
func TestReconcile_UnprogrammablePolicyKeepsExistingMemberships(t *testing.T) {
	ctx := context.Background()
	rec, m := newTestReconciler(t)

	intent := freshIntent(t)
	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile #1: %v", err)
	}
	if m.Ports["port-eni-a"] == nil {
		t.Fatal("ENI port not created")
	}

	// A pass whose EnsureSGPortGroup failed sees the port group as absent while
	// the LSP is still joined to it in OVN.
	actual := newActualState()
	var res passResult
	rec.applyPorts(ctx, intent, actual, false, &res)
	if len(res.failures) == 0 {
		t.Error("applyPorts reported no failure for an unprogrammable policy")
	}

	names, err := m.ListPortGroupsForPort(ctx, "port-eni-a")
	if err != nil {
		t.Fatalf("ListPortGroupsForPort: %v", err)
	}
	if len(names) == 0 {
		t.Error("live port stripped of every port group — it is now unrestricted")
	}
}

// Once applySGs catches up, the next pass creates the port and joins it: the
// refusal is a window that heals, not a permanent block.
func TestReconcile_RefusedPortHealsOncePortGroupExists(t *testing.T) {
	ctx := context.Background()
	rec, m := newTestReconciler(t)

	intent := freshIntent(t)
	withoutSG := intent
	withoutSG.SGs = map[string]policy.SGSpec{}
	if err := rec.Reconcile(ctx, withoutSG); !errors.Is(err, ErrPassIncomplete) {
		t.Fatalf("Reconcile #1 err = %v, want ErrPassIncomplete", err)
	}
	if _, ok := m.Ports["port-eni-a"]; ok {
		t.Fatal("port created before its port group existed")
	}

	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile #2: %v", err)
	}
	storedPort := m.Ports["port-eni-a"]
	if storedPort == nil {
		t.Fatal("port not created once its port group existed")
	}
	pg := m.PortGroups[topology.SecurityGroupPortGroup("sg-a")]
	if pg == nil || !slices.Contains(pg.Ports, storedPort.UUID) {
		t.Error("healed port not joined to its SG port group")
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

// The prune pass must sweep dnat_and_snat rows whose stamped owning ENI is absent
// from intent (leaked across dead VPCs by a lost vpc.delete-nat), keying on the
// union of intent port names and EIP port names, while leaving live rows intact.
func TestReconcile_PruneOrphanEIPs_SweepsAbsentOwners(t *testing.T) {
	r, m := newTestReconciler(t)
	ctx := context.Background()

	deadRouter := topology.VPCRouter("vpc-dead")
	if err := m.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{Name: deadRouter}); err != nil {
		t.Fatalf("CreateLogicalRouter: %v", err)
	}
	if err := m.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{Name: topology.VPCRouter("vpc-a")}); err != nil {
		t.Fatalf("CreateLogicalRouter live: %v", err)
	}
	// Live row: owned by an ENI present in intent.Ports.
	if err := m.AddNAT(ctx, topology.VPCRouter("vpc-a"), &nbdb.NAT{
		Type: "dnat_and_snat", ExternalIP: "192.168.1.10", LogicalIP: "10.0.1.10",
		ExternalIDs: map[string]string{"spinifex:logical_port": topology.Port("eni-a")},
	}); err != nil {
		t.Fatalf("AddNAT live: %v", err)
	}
	// Orphan row: owner ENI is gone from intent.
	if err := m.AddNAT(ctx, deadRouter, &nbdb.NAT{
		Type: "dnat_and_snat", ExternalIP: "192.168.1.11", LogicalIP: "172.31.0.4",
		ExternalIDs: map[string]string{"spinifex:logical_port": topology.Port("eni-dead")},
	}); err != nil {
		t.Fatalf("AddNAT orphan: %v", err)
	}

	intent := freshIntent(t) // Ports has eni-a only
	r.pruneOrphanEIPs(ctx, intent, &passResult{})

	if findNATByExternal(m, "dnat_and_snat", "192.168.1.10") == nil {
		t.Errorf("live EIP row must survive the prune")
	}
	if findNATByExternal(m, "dnat_and_snat", "192.168.1.11") != nil {
		t.Errorf("orphan EIP row must be pruned")
	}
}

// A guest launched during a long apply phase has a live dnat_and_snat row that the
// start-of-pass snapshot does not carry. When a fresh-intent loader is wired, the
// prune must union its ports and spare that row, while still sweeping a genuine orphan.
func TestReconcile_PruneOrphanEIPs_SparesMidPassLaunch(t *testing.T) {
	r, m := newTestReconciler(t)
	ctx := context.Background()

	if err := m.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{Name: topology.VPCRouter("vpc-a")}); err != nil {
		t.Fatalf("CreateLogicalRouter live: %v", err)
	}
	if err := m.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{Name: topology.VPCRouter("vpc-dead")}); err != nil {
		t.Fatalf("CreateLogicalRouter dead: %v", err)
	}
	// Row for a guest launched after the snapshot: absent from the passed intent,
	// present only in the fresh re-read.
	if err := m.AddNAT(ctx, topology.VPCRouter("vpc-a"), &nbdb.NAT{
		Type: "dnat_and_snat", ExternalIP: "192.168.1.20", LogicalIP: "10.0.1.20",
		ExternalIDs: map[string]string{"spinifex:logical_port": topology.Port("eni-fresh")},
	}); err != nil {
		t.Fatalf("AddNAT fresh: %v", err)
	}
	// Genuine orphan: absent from both the snapshot and the fresh re-read.
	if err := m.AddNAT(ctx, topology.VPCRouter("vpc-dead"), &nbdb.NAT{
		Type: "dnat_and_snat", ExternalIP: "192.168.1.11", LogicalIP: "172.31.0.4",
		ExternalIDs: map[string]string{"spinifex:logical_port": topology.Port("eni-dead")},
	}); err != nil {
		t.Fatalf("AddNAT orphan: %v", err)
	}

	// The passed intent (start-of-pass snapshot) predates the launch: eni-a only.
	snapshot := freshIntent(t)
	// The fresh re-read sees the mid-pass launch.
	r.reloadIntent = func(context.Context) (IntentState, error) {
		fresh := freshIntent(t)
		fresh.Ports["eni-fresh"] = topology.PortSpec{
			PortID: "eni-fresh", SubnetID: "subnet-a", VPCID: "vpc-a",
			PrivateIP: netip.MustParseAddr("10.0.1.20"),
		}
		return fresh, nil
	}

	r.pruneOrphanEIPs(ctx, snapshot, &passResult{})

	if findNATByExternal(m, "dnat_and_snat", "192.168.1.20") == nil {
		t.Errorf("mid-pass launch row must survive the prune via the fresh re-read")
	}
	if findNATByExternal(m, "dnat_and_snat", "192.168.1.11") != nil {
		t.Errorf("genuine orphan row must still be pruned")
	}
}

// When the fresh-intent re-read fails, the prune is skipped wholesale rather than
// risk sweeping a live row against a snapshot known to be stale.
func TestReconcile_PruneOrphanEIPs_SkipsOnReloadError(t *testing.T) {
	r, m := newTestReconciler(t)
	ctx := context.Background()

	if err := m.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{Name: topology.VPCRouter("vpc-dead")}); err != nil {
		t.Fatalf("CreateLogicalRouter dead: %v", err)
	}
	if err := m.AddNAT(ctx, topology.VPCRouter("vpc-dead"), &nbdb.NAT{
		Type: "dnat_and_snat", ExternalIP: "192.168.1.11", LogicalIP: "172.31.0.4",
		ExternalIDs: map[string]string{"spinifex:logical_port": topology.Port("eni-dead")},
	}); err != nil {
		t.Fatalf("AddNAT orphan: %v", err)
	}

	r.reloadIntent = func(context.Context) (IntentState, error) {
		return IntentState{}, errors.New("kv unavailable")
	}
	r.pruneOrphanEIPs(ctx, freshIntent(t), &passResult{})

	if findNATByExternal(m, "dnat_and_snat", "192.168.1.11") == nil {
		t.Errorf("prune must be skipped when the fresh re-read fails")
	}
}

// An SG created after the pass snapshotted its intent is live but missing from
// that snapshot. Deleting its port group breaks every later ENI create in the VPC
// ("port group not found"), so the prune must union a fresh re-read into the keep
// set while still sweeping a genuine orphan.
func TestReconcile_ApplySGs_SparesMidPassSecurityGroup(t *testing.T) {
	r, m := newTestReconciler(t)
	ctx := context.Background()

	freshPG := topology.SecurityGroupPortGroup("sg-fresh")
	deadPG := topology.SecurityGroupPortGroup("sg-dead")
	for _, pgName := range []string{freshPG, deadPG} {
		if _, _, err := m.EnsurePortGroup(ctx, pgName, nil); err != nil {
			t.Fatalf("EnsurePortGroup %s: %v", pgName, err)
		}
	}

	// The re-read sees the SG created mid-pass; the snapshot carries sg-a only.
	r.reloadIntent = func(context.Context) (IntentState, error) {
		fresh := freshIntent(t)
		fresh.SGs["sg-fresh"] = policy.SGSpec{GroupID: "sg-fresh", VPCID: "vpc-a"}
		return fresh, nil
	}

	if err := r.reconcile(ctx, freshIntent(t), true); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if _, ok := m.PortGroups[freshPG]; !ok {
		t.Errorf("mid-pass SG port group %s must survive the prune via the fresh re-read", freshPG)
	}
	if _, ok := m.PortGroups[deadPG]; ok {
		t.Errorf("genuine orphan port group %s must still be pruned", deadPG)
	}
}

// When the fresh-intent re-read fails, the port group sweep is skipped wholesale
// rather than risk deleting a live SG against a snapshot known to be stale.
func TestReconcile_ApplySGs_SkipsPruneOnReloadError(t *testing.T) {
	r, m := newTestReconciler(t)
	ctx := context.Background()

	deadPG := topology.SecurityGroupPortGroup("sg-dead")
	if _, _, err := m.EnsurePortGroup(ctx, deadPG, nil); err != nil {
		t.Fatalf("EnsurePortGroup: %v", err)
	}
	r.reloadIntent = func(context.Context) (IntentState, error) {
		return IntentState{}, errors.New("kv unavailable")
	}

	res := &passResult{}
	r.applySGs(ctx, freshIntent(t), scanOrFail(t, ctx, r), true, res)

	if _, ok := m.PortGroups[deadPG]; !ok {
		t.Errorf("prune must be skipped when the fresh re-read fails")
	}
	if len(res.failures) == 0 {
		t.Errorf("a skipped prune must mark the pass incomplete so the loop requeues")
	}
}

// Same window against guest LSPs: an ENI created mid-pass is absent from the
// snapshot, and pruning its port strands the guest with no datapath.
func TestReconcile_PruneOrphanPorts_SparesMidPassENI(t *testing.T) {
	r, m := newTestReconciler(t)
	ctx := context.Background()

	if err := r.reconcile(ctx, freshIntent(t), false); err != nil {
		t.Fatalf("reconcile (seed): %v", err)
	}
	for _, eniID := range []string{"eni-fresh", "eni-dead"} {
		lsp := &nbdb.LogicalSwitchPort{
			Name: topology.Port(eniID),
			ExternalIDs: map[string]string{
				"spinifex:eni_id":    eniID,
				"spinifex:subnet_id": "subnet-a",
				"spinifex:vpc_id":    "vpc-a",
			},
		}
		if err := m.CreateLogicalSwitchPort(ctx, topology.SubnetSwitch("subnet-a"), lsp); err != nil {
			t.Fatalf("seed LSP %s: %v", eniID, err)
		}
	}

	mac, _ := net.ParseMAC("02:00:00:00:00:02")
	r.reloadIntent = func(context.Context) (IntentState, error) {
		fresh := freshIntent(t)
		fresh.Ports["eni-fresh"] = topology.PortSpec{
			PortID: "eni-fresh", SubnetID: "subnet-a", VPCID: "vpc-a",
			PrivateIP: netip.MustParseAddr("10.0.1.20"), MAC: mac, SGIDs: []string{"sg-a"},
		}
		return fresh, nil
	}

	r.pruneOrphanPorts(ctx, freshIntent(t), &passResult{})

	if _, ok := m.Ports[topology.Port("eni-fresh")]; !ok {
		t.Errorf("mid-pass ENI port must survive the prune via the fresh re-read")
	}
	if _, ok := m.Ports[topology.Port("eni-dead")]; ok {
		t.Errorf("genuine orphan ENI port must still be pruned")
	}
}

// A failed re-read skips the guest LSP sweep entirely, same as the SG and EIP paths.
func TestReconcile_PruneOrphanPorts_SkipsOnReloadError(t *testing.T) {
	r, m := newTestReconciler(t)
	ctx := context.Background()

	if err := r.reconcile(ctx, freshIntent(t), false); err != nil {
		t.Fatalf("reconcile (seed): %v", err)
	}
	orphan := &nbdb.LogicalSwitchPort{
		Name: topology.Port("eni-dead"),
		ExternalIDs: map[string]string{
			"spinifex:eni_id":    "eni-dead",
			"spinifex:subnet_id": "subnet-a",
			"spinifex:vpc_id":    "vpc-a",
		},
	}
	if err := m.CreateLogicalSwitchPort(ctx, topology.SubnetSwitch("subnet-a"), orphan); err != nil {
		t.Fatalf("seed orphan LSP: %v", err)
	}

	r.reloadIntent = func(context.Context) (IntentState, error) {
		return IntentState{}, errors.New("kv unavailable")
	}
	res := &passResult{}
	r.pruneOrphanPorts(ctx, freshIntent(t), res)

	if _, ok := m.Ports[topology.Port("eni-dead")]; !ok {
		t.Errorf("prune must be skipped when the fresh re-read fails")
	}
	if len(res.failures) == 0 {
		t.Errorf("a skipped prune must mark the pass incomplete so the loop requeues")
	}
}

// racingOVN creates an ENI's LSP part-way through the sweep's own listing, so
// the returned rows include a port that did not exist when the sweep started.
type racingOVN struct {
	*mock.Client

	listed bool
}

func (o *racingOVN) ListLogicalSwitchPorts(ctx context.Context) ([]nbdb.LogicalSwitchPort, error) {
	if !o.listed {
		lsp := &nbdb.LogicalSwitchPort{
			Name: topology.Port("eni-raced"),
			ExternalIDs: map[string]string{
				"spinifex:eni_id":    "eni-raced",
				"spinifex:subnet_id": "subnet-a",
				"spinifex:vpc_id":    "vpc-a",
			},
		}
		if err := o.CreateLogicalSwitchPort(ctx, topology.SubnetSwitch("subnet-a"), lsp); err != nil {
			return nil, err
		}
		o.listed = true
	}
	return o.Client.ListLogicalSwitchPorts(ctx)
}

// The create path writes the ENI to KV before it creates the LSP, so intent read
// after the listing covers every row in it. Re-reading first reopens the window:
// the new port is listed but absent from both snapshots and gets swept.
func TestReconcile_PruneOrphanPorts_ReloadsAfterListing(t *testing.T) {
	r, m := newTestReconciler(t)
	ctx := context.Background()

	if err := r.reconcile(ctx, freshIntent(t), false); err != nil {
		t.Fatalf("reconcile (seed): %v", err)
	}
	racing := &racingOVN{Client: m}
	r.ovn = racing

	mac, _ := net.ParseMAC("02:00:00:00:00:03")
	r.reloadIntent = func(context.Context) (IntentState, error) {
		fresh := freshIntent(t)
		// The KV record exists only once the LSP does, mirroring KV-then-OVN.
		if racing.listed {
			fresh.Ports["eni-raced"] = topology.PortSpec{
				PortID: "eni-raced", SubnetID: "subnet-a", VPCID: "vpc-a",
				PrivateIP: netip.MustParseAddr("10.0.1.30"), MAC: mac, SGIDs: []string{"sg-a"},
			}
		}
		return fresh, nil
	}

	res := &passResult{}
	r.pruneOrphanPorts(ctx, freshIntent(t), res)

	if _, ok := m.Ports[topology.Port("eni-raced")]; !ok {
		t.Errorf("ENI created during the listing must survive; re-read the intent after listing, not before")
	}
	if len(res.failures) != 0 {
		t.Errorf("a clean sweep must not mark the pass incomplete, got %d failure(s)", len(res.failures))
	}
}

// scanOrFail snapshots the mock's OVN state the way a pass does.
func scanOrFail(t *testing.T, ctx context.Context, r *reconciler) ActualState {
	t.Helper()
	actual, err := scanActual(ctx, r.ovn)
	if err != nil {
		t.Fatalf("scanActual: %v", err)
	}
	return actual
}

// findNATByExternal returns the first NAT row matching (type, externalIP), or nil.
func findNATByExternal(m *mock.Client, natType, externalIP string) *nbdb.NAT {
	for _, n := range m.NATs {
		if n.Type == natType && n.ExternalIP == externalIP {
			return n
		}
	}
	return nil
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

// A DHCPOptions row is created once and then never revisited by the original
// apply path, so toggling network.ipsec_enabled on a live cluster left the
// create-time MTU in place forever. Going from off to on that means a subnet
// still advertising 1442 on an ESP path, which blackholes large segments.
func TestApplySubnets_ConvergesDriftedDHCPOptions(t *testing.T) {
	rec, m := newTestReconciler(t)
	ctx := context.Background()
	intent := freshIntent(t)

	// First pass creates the row. The test reconciler leaves IPSecDisabled
	// unset, so it lands on the conservative MTU.
	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	before, err := m.FindDHCPOptionsByExternalID(ctx, "spinifex:subnet_id", "subnet-a")
	if err != nil {
		t.Fatalf("FindDHCPOptionsByExternalID: %v", err)
	}
	if got := before.Options["mtu"]; got != "1408" {
		t.Fatalf("mtu after create = %q, want \"1408\"", got)
	}

	// Operator disables IPsec; the next pass must widen the advertised MTU.
	rec.ipsecEnabled = false
	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile after IPsec toggle: %v", err)
	}
	after, err := m.FindDHCPOptionsByExternalID(ctx, "spinifex:subnet_id", "subnet-a")
	if err != nil {
		t.Fatalf("FindDHCPOptionsByExternalID: %v", err)
	}
	if got := after.Options["mtu"]; got != "1442" {
		t.Errorf("mtu after disabling IPsec = %q, want \"1442\" — DHCP options are not converged, only created", got)
	}
	if after.UUID != before.UUID {
		t.Errorf("DHCP options row was replaced (%s -> %s), want an in-place update", before.UUID, after.UUID)
	}
}

// stubIGW forces AttachIGW to fail while delegating everything else to a real
// manager, so a pass fails exactly one resource and the rest still applies.
type stubIGW struct {
	external.IGWManager

	attachErr error
}

func (s *stubIGW) AttachIGW(ctx context.Context, spec external.IGWSpec) error {
	if s.attachErr != nil {
		return s.attachErr
	}
	return s.IGWManager.AttachIGW(ctx, spec)
}

// igwIntent is freshIntent plus an IGW for vpc-a, the resource whose failed
// attach a pass used to swallow.
func igwIntent(t *testing.T) IntentState {
	t.Helper()
	intent := freshIntent(t)
	intent.IGWs["vpc-a"] = external.IGWSpec{VPCID: "vpc-a", InternetGatewayID: "igw-a", RecordKey: "acct.igw-a"}
	return intent
}

// The control plane reports an attachment only once a pass confirms it, so the
// hook must fire on success and stay silent on failure — a record marked
// attached after a failed attach is the bug this replaced.
func TestReconcile_MarksIGWAttachedOnlyOnSuccess(t *testing.T) {
	ctx := context.Background()

	t.Run("success", func(t *testing.T) {
		rec, _ := newTestReconciler(t)
		var keys []string
		rec.markAttached = func(_ context.Context, key string) error {
			keys = append(keys, key)
			return nil
		}
		if err := rec.Reconcile(ctx, igwIntent(t)); err != nil {
			t.Fatalf("Reconcile = %v, want nil", err)
		}
		if want := []string{"acct.igw-a"}; !slices.Equal(keys, want) {
			t.Errorf("markAttached keys = %v, want %v — a converged attach must be reported "+
				"or DescribeInternetGateways never stops calling it pending", keys, want)
		}
	})

	t.Run("failure", func(t *testing.T) {
		rec, _ := newTestReconciler(t)
		rec.igw = &stubIGW{IGWManager: rec.igw, attachErr: errors.New("dhcp gw-lrp acquire: context deadline exceeded")}
		called := false
		rec.markAttached = func(context.Context, string) error {
			called = true
			return nil
		}
		if err := rec.Reconcile(ctx, igwIntent(t)); !errors.Is(err, ErrPassIncomplete) {
			t.Fatalf("Reconcile = %v, want ErrPassIncomplete", err)
		}
		if called {
			t.Error("markAttached called after a failed attach: the API would report a gateway that is not up")
		}
	})

	// vpcd wires the hook, unit callers do not; a nil hook must not panic.
	t.Run("nil hook", func(t *testing.T) {
		rec, _ := newTestReconciler(t)
		rec.markAttached = nil
		if err := rec.Reconcile(ctx, igwIntent(t)); err != nil {
			t.Fatalf("Reconcile = %v, want nil with no hook wired", err)
		}
	})
}

// A failed AttachIGW must surface as ErrPassIncomplete rather than a nil return:
// a swallowed failure is indistinguishable from a converged pass, so nothing
// retries it until the next drift tick.
func TestReconcile_FailedAttachIGWReportsPassIncomplete(t *testing.T) {
	rec, m := newTestReconciler(t)
	ctx := context.Background()
	attachErr := errors.New("allocate gateway LRP IP: dhcp gw-lrp acquire: context deadline exceeded")
	rec.igw = &stubIGW{IGWManager: rec.igw, attachErr: attachErr}
	// Without a chassis the rebind returns on its first line, which would make
	// the assertion below pass whether or not the failure short-circuits it.
	rec.chassis = []string{"chassis-1"}

	err := rec.Reconcile(ctx, igwIntent(t))
	if !errors.Is(err, ErrPassIncomplete) {
		t.Fatalf("Reconcile = %v, want ErrPassIncomplete — a swallowed AttachIGW failure "+
			"leaves the drift loop unable to tell a converged pass from a broken one", err)
	}
	// The rest of the pass still applies: only the IGW is unconverged.
	if _, ok := m.Ports[topology.Port("eni-a")]; !ok {
		t.Errorf("ENI port missing: a failed IGW attach must not abort the pass")
	}
	if _, ok := m.RouterPorts[topology.GatewayRouterPort("vpc-a")]; ok {
		t.Errorf("gateway LRP present after a failed attach")
	}
	if m.SetGatewayChassisCalls != 0 {
		t.Errorf("SetGatewayChassis called %d times after a failed attach, want 0 — "+
			"rebinding chassis onto a gateway LRP that was never created", m.SetGatewayChassisCalls)
	}
}

// The converged path must stay clean: a pass with the same intent and a working
// AttachIGW returns nil, so the drift loop resets to DriftInterval.
func TestReconcile_AttachedIGWReportsConverged(t *testing.T) {
	rec, m := newTestReconciler(t)
	ctx := context.Background()
	rec.chassis = []string{"chassis-1"}

	if err := rec.Reconcile(ctx, igwIntent(t)); err != nil {
		t.Fatalf("Reconcile = %v, want nil", err)
	}
	if _, ok := m.RouterPorts[topology.GatewayRouterPort("vpc-a")]; !ok {
		t.Errorf("gateway LRP missing after a successful attach")
	}
	if m.SetGatewayChassisCalls == 0 {
		t.Errorf("SetGatewayChassis never called after a successful attach: the rebind " +
			"must run, or a rebooted chassis never reclaims the gateway")
	}
}

// stubNAT forces AddEIP to fail, delegating everything else.
type stubNAT struct {
	policy.NATManager

	addErr error
}

func (s *stubNAT) AddEIP(ctx context.Context, spec policy.EIPSpec) error {
	if s.addErr != nil {
		return s.addErr
	}
	return s.NATManager.AddEIP(ctx, spec)
}

// The incomplete signal is a per-pass contract, not an IGW special case: a stage
// other than applyIGWs failing must report itself the same way.
func TestReconcile_FailedAddEIPReportsPassIncomplete(t *testing.T) {
	rec, _ := newTestReconciler(t)
	ctx := context.Background()
	rec.nat = &stubNAT{NATManager: rec.nat, addErr: errors.New("ovsdb unreachable")}

	intent := freshIntent(t)
	intent.EIPs["10.0.1.10"] = policy.EIPSpec{
		VPCID: "vpc-a", ExternalIP: "192.168.1.10", LogicalIP: "10.0.1.10",
		PortName: topology.Port("eni-a"),
	}
	if err := rec.Reconcile(ctx, intent); !errors.Is(err, ErrPassIncomplete) {
		t.Fatalf("Reconcile = %v, want ErrPassIncomplete for a failed AddEIP", err)
	}
}

// The convergence summary counts failures per class, so an operator reading one
// line knows which stage did not converge and how widely.
func TestPassResult_SummaryCountsPerClass(t *testing.T) {
	res := &passResult{}
	res.fail(classIGW, "vpc-a", errors.New("x"))
	res.fail(classEIP, "192.168.1.10", errors.New("y"))
	res.fail(classEIP, "192.168.1.11", errors.New("z"))

	want := []any{"unconverged", 3, classEIP, 2, classIGW, 1}
	if got := res.summaryKV(); !slices.Equal(got, want) {
		t.Errorf("summaryKV() = %v, want %v", got, want)
	}
}
