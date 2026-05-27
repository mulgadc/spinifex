package reconcile

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

// newTestReconciler builds a reconciler around the in-memory OVN mock. NAT
// mode defaults to distributed (physical uplink) so EIP rules carry
// LogicalPort/MAC.
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
	addACLsBefore := m.AddACLCalls

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
	// EnsureSG re-applies ACLs every call (replace-all semantics); both
	// passes add the same 4 infra + 0 tenant ACLs, so total adds doubles.
	if m.AddACLCalls < addACLsBefore {
		t.Errorf("second reconcile fewer AddACL calls than first — replace-all semantics regressed")
	}
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

// TestReconcile_ApplyOnlyKeepsOrphanPortGroup guards the vpcd startup path:
// ReconcileApplyOnly must not prune managed sg_* port groups even when intent
// is empty. The race it protects against is daemon EnsureDefaultVPC on a peer
// node having written the SG KV row + driven a peer subscriber to create the
// port group while this node's leader-gated intent load still returned no
// SGs. Drift (full Reconcile) cleans up legitimate orphans on the next tick.
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

	// Full Reconcile must still prune — drift loop's contract.
	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := m.PortGroups["sg_orphan"]; ok {
		t.Errorf("Reconcile failed to prune orphan after ApplyOnly path")
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

	// AttachIGW creates a gateway LRP on the VPC router, so seed the VPC
	// router via a VPC-only reconcile first.
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
	// Seed an external switch + gateway LRP so the apply path takes the
	// rebind branch rather than AttachIGW.
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

	if _, ok := m.Switches[topology.ExternalSwitch("vpc-a")]; !ok {
		t.Errorf("external switch not created by reconciler AttachIGW path")
	}
	gwPort := topology.GatewayRouterPort("vpc-a")
	if _, ok := m.RouterPorts[gwPort]; !ok {
		t.Errorf("gateway LRP %s not created", gwPort)
	}

	// Second pass must be idempotent — AttachIGW's first-line short-circuit
	// fires once the external switch exists, so total switch count stays put.
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

	// Drift: add a second SG to the intent, expect membership update.
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
		IGWs:   map[string]external.IGWSpec{},
		EIPs:   map[string]policy.EIPSpec{},
		NATGWs: map[string]policy.NATGWSpec{},
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	slices.Sort(out)
	return out
}
