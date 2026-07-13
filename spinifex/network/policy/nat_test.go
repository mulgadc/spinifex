package policy

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/network/host"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

func seedGatewayPort(t *testing.T, m *mock.Client, vpcID, mac string) {
	t.Helper()
	require.NoError(t, m.CreateLogicalRouterPort(context.Background(),
		topology.VPCRouter(vpcID),
		&nbdb.LogicalRouterPort{Name: topology.GatewayRouterPort(vpcID), MAC: mac}))
}

func seedRouter(t *testing.T, m *mock.Client, vpcID string) string {
	t.Helper()
	name := topology.VPCRouter(vpcID)
	require.NoError(t, m.CreateLogicalRouter(context.Background(), &nbdb.LogicalRouter{Name: name}))
	return name
}

func findNAT(m *mock.Client, natType, logicalIP string) *nbdb.NAT {
	for _, n := range m.NATs {
		if n.Type == natType && n.LogicalIP == logicalIP {
			return n
		}
	}
	return nil
}

func countNAT(m *mock.Client, natType, logicalIP string) int {
	var n int
	for _, nat := range m.NATs {
		if nat.Type == natType && nat.LogicalIP == logicalIP {
			n++
		}
	}
	return n
}

func TestNATModeFromUplinkMode(t *testing.T) {
	assert.Equal(t, NATModeDistributed, NATModeFromUplinkMode(host.UplinkModePhysical))
	assert.Equal(t, NATModeCentralized, NATModeFromUplinkMode(host.UplinkModeVeth))
	assert.Equal(t, NATModeRouted, NATModeFromUplinkMode(host.UplinkModeRouted))
	assert.Equal(t, NATModeUnknown, NATModeFromUplinkMode(host.UplinkModeUnknown))
}

func TestNewNATManager_RejectsUnknownMode(t *testing.T) {
	_, err := NewNATManager(mock.New(), NATModeUnknown)
	require.Error(t, err)
}

func TestNATManager_AddEIP_FlowsBarrier_Fires(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	var calls int
	nm, err := NewNATManager(m, NATModeDistributed, WithFlowsBarrier(func() error {
		calls++
		return nil
	}))
	require.NoError(t, err)

	require.NoError(t, nm.AddEIP(ctx, EIPSpec{
		VPCID:      "vpc-1",
		ExternalIP: "1.2.3.4",
		LogicalIP:  "10.0.0.5",
		PortName:   "port-eni-abc",
		MAC:        "aa:bb:cc:dd:ee:ff",
	}))
	assert.Equal(t, 1, calls, "FlowsBarrier must fire once per AddEIP")
}

func TestNATManager_AddEIP_IdempotentSkip(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	var barrierCalls int
	nm, err := NewNATManager(m, NATModeDistributed, WithFlowsBarrier(func() error {
		barrierCalls++
		return nil
	}))
	require.NoError(t, err)

	spec := EIPSpec{
		VPCID: "vpc-1", ExternalIP: "1.2.3.4", LogicalIP: "10.0.0.5",
		PortName: "port-eni-abc", MAC: "aa:bb:cc:dd:ee:ff",
	}
	require.NoError(t, nm.AddEIP(ctx, spec))
	require.Equal(t, 1, barrierCalls, "first AddEIP must fire barrier")
	firstUUID := findNAT(m, "dnat_and_snat", spec.LogicalIP).UUID

	// Second AddEIP with the same spec must skip delete-then-add. UUID
	// staying constant confirms the existing row was not replaced.
	require.NoError(t, nm.AddEIP(ctx, spec))
	assert.Equal(t, firstUUID, findNAT(m, "dnat_and_snat", spec.LogicalIP).UUID,
		"idempotent re-add must reuse the existing NAT row")
	assert.Equal(t, 1, barrierCalls, "FlowsBarrier must not fire on idempotent skip")
}

func TestNATManager_AddEIP_IdempotentSkip_RePrimesReachability(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	var primed []EIPSpec
	var barrierCalls int
	nm, err := NewNATManager(m, NATModeDistributed,
		WithFlowsBarrier(func() error { barrierCalls++; return nil }),
		WithNeighPrimer(func(eip EIPSpec) error { primed = append(primed, eip); return nil }))
	require.NoError(t, err)

	spec := EIPSpec{
		VPCID: "vpc-1", ExternalIP: "1.2.3.4", LogicalIP: "10.0.0.5",
		PortName: "port-eni-abc", MAC: "aa:bb:cc:dd:ee:ff",
	}
	require.NoError(t, nm.AddEIP(ctx, spec))
	// Re-attach same EIP (stop->start): row write is skipped but reachability
	// must be re-primed or the host neigh goes dark until ARP times out.
	require.NoError(t, nm.AddEIP(ctx, spec))

	assert.Equal(t, 1, barrierCalls, "FlowsBarrier must not fire on idempotent skip")
	assert.Equal(t, []EIPSpec{spec, spec}, primed, "neighbour prime must re-fire on the idempotent-skip path")
}

func TestNATManager_NeighPrime_OnDistributedAttach_FlushOnDetach(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	var primed []EIPSpec
	var flushed []string
	nm, err := NewNATManager(m, NATModeDistributed,
		WithNeighPrimer(func(eip EIPSpec) error {
			primed = append(primed, eip)
			return nil
		}),
		WithNeighFlusher(func(externalIP string) error {
			flushed = append(flushed, externalIP)
			return nil
		}))
	require.NoError(t, err)

	spec := EIPSpec{
		VPCID: "vpc-1", ExternalIP: "1.2.3.4", LogicalIP: "10.0.0.5",
		PortName: "port-eni-abc", MAC: "aa:bb:cc:dd:ee:ff",
	}
	require.NoError(t, nm.AddEIP(ctx, spec))
	require.NoError(t, nm.DeleteEIP(ctx, "vpc-1", "1.2.3.4", "10.0.0.5", ""))

	require.Equal(t, []EIPSpec{spec}, primed,
		"distributed attach must prime the host neighbour with the external_mac, not flush")
	assert.Equal(t, []string{"1.2.3.4"}, flushed,
		"detach must still flush the released external IP")
}

func TestNATManager_NeighPrime_OnCentralizedAttach(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	seedGatewayPort(t, m, "vpc-1", "02:gw:00:00:00:01")
	var primed []EIPSpec
	var flushed []string
	nm, err := NewNATManager(m, NATModeCentralized,
		WithNeighPrimer(func(eip EIPSpec) error {
			primed = append(primed, eip)
			return nil
		}),
		WithNeighFlusher(func(externalIP string) error {
			flushed = append(flushed, externalIP)
			return nil
		}))
	require.NoError(t, err)

	require.NoError(t, nm.AddEIP(ctx, EIPSpec{
		VPCID: "vpc-1", ExternalIP: "1.2.3.4", LogicalIP: "10.0.0.5",
	}))
	require.Len(t, primed, 1,
		"centralized attach must prime the host neighbour with the gateway port MAC")
	assert.Equal(t, "02:gw:00:00:00:01", primed[0].MAC,
		"centralized prime must use the VPC gateway router-port MAC")
	assert.Empty(t, flushed, "a successful prime must not also flush")
}

func TestNATManager_NeighFlush_CentralizedNoGatewayMAC(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1") // no gateway port seeded → MAC unresolvable
	var primed []EIPSpec
	var flushed []string
	nm, err := NewNATManager(m, NATModeCentralized,
		WithNeighPrimer(func(eip EIPSpec) error {
			primed = append(primed, eip)
			return nil
		}),
		WithNeighFlusher(func(externalIP string) error {
			flushed = append(flushed, externalIP)
			return nil
		}))
	require.NoError(t, err)

	require.NoError(t, nm.AddEIP(ctx, EIPSpec{
		VPCID: "vpc-1", ExternalIP: "1.2.3.4", LogicalIP: "10.0.0.5",
	}))
	assert.Empty(t, primed, "no gateway MAC to prime")
	assert.Equal(t, []string{"1.2.3.4"}, flushed,
		"centralized attach must fall back to a neighbour flush when no MAC resolves")
}

func TestNATManager_NeighHooks_FailureNonFatal(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	nm, err := NewNATManager(m, NATModeDistributed,
		WithNeighPrimer(func(EIPSpec) error { return assert.AnError }),
		WithNeighFlusher(func(string) error { return assert.AnError }))
	require.NoError(t, err)

	require.NoError(t, nm.AddEIP(ctx, EIPSpec{
		VPCID: "vpc-1", ExternalIP: "1.2.3.4", LogicalIP: "10.0.0.5",
		PortName: "port-eni-abc", MAC: "aa:bb:cc:dd:ee:ff",
	}), "neigh prime failure must not propagate from AddEIP")
	require.NoError(t, nm.DeleteEIP(ctx, "vpc-1", "1.2.3.4", "10.0.0.5", ""),
		"neigh flush failure must not propagate from DeleteEIP")
}

func TestNATManager_AddEIP_NoBarrier_Default(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	nm, err := NewNATManager(m, NATModeDistributed)
	require.NoError(t, err)

	require.NoError(t, nm.AddEIP(ctx, EIPSpec{
		VPCID: "vpc-1", ExternalIP: "1.2.3.4", LogicalIP: "10.0.0.5",
	}))
}

func TestNATManager_AddEIP_Distributed_SetsPortAndMAC(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	nm, err := NewNATManager(m, NATModeDistributed)
	require.NoError(t, err)

	require.NoError(t, nm.AddEIP(ctx, EIPSpec{
		VPCID:      "vpc-1",
		ExternalIP: "1.2.3.4",
		LogicalIP:  "10.0.0.5",
		PortName:   "port-eni-abc",
		MAC:        "aa:bb:cc:dd:ee:ff",
	}))

	got := findNAT(m, "dnat_and_snat", "10.0.0.5")
	require.NotNil(t, got)
	require.NotNil(t, got.LogicalPort)
	require.NotNil(t, got.ExternalMAC)
	assert.Equal(t, "port-eni-abc", *got.LogicalPort)
	assert.Equal(t, "aa:bb:cc:dd:ee:ff", *got.ExternalMAC)
}

func TestNATManager_AddEIP_Centralized_LeavesPortAndMACUnset(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	nm, err := NewNATManager(m, NATModeCentralized)
	require.NoError(t, err)

	require.NoError(t, nm.AddEIP(ctx, EIPSpec{
		VPCID:      "vpc-1",
		ExternalIP: "1.2.3.4",
		LogicalIP:  "10.0.0.5",
		PortName:   "port-eni-abc",
		MAC:        "aa:bb:cc:dd:ee:ff",
	}))

	got := findNAT(m, "dnat_and_snat", "10.0.0.5")
	require.NotNil(t, got)
	assert.Nil(t, got.LogicalPort)
	assert.Nil(t, got.ExternalMAC)
}

func TestNATManager_AddEIP_RemovesStaleRuleOnOtherRouter(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-old")
	seedRouter(t, m, "vpc-new")
	nm, err := NewNATManager(m, NATModeDistributed)
	require.NoError(t, err)

	// Stale rule still attached to vpc-old's router for the same external IP.
	require.NoError(t, nm.AddEIP(ctx, EIPSpec{
		VPCID:      "vpc-old",
		ExternalIP: "1.2.3.4",
		LogicalIP:  "10.0.0.5",
		PortName:   "port-a", MAC: "aa:aa:aa:aa:aa:aa",
	}))
	// Reuse the EIP in vpc-new.
	require.NoError(t, nm.AddEIP(ctx, EIPSpec{
		VPCID:      "vpc-new",
		ExternalIP: "1.2.3.4",
		LogicalIP:  "10.1.0.7",
		PortName:   "port-b", MAC: "bb:bb:bb:bb:bb:bb",
	}))

	// Only the new rule survives.
	var matching []*nbdb.NAT
	for _, n := range m.NATs {
		if n.Type == "dnat_and_snat" && n.ExternalIP == "1.2.3.4" {
			matching = append(matching, n)
		}
	}
	require.Len(t, matching, 1)
	assert.Equal(t, "10.1.0.7", matching[0].LogicalIP)
}

// A NEW ENI taking over the same external+logical IP (private-IP reuse) must
// re-point the dnat_and_snat row even in centralised mode, where the native
// LogicalPort column is empty so only the spinifex:logical_port external-id can
// discriminate the owner. Without the re-point the datapath targets the dead port.
func TestNATManager_AddEIP_RePointsOnOwnerChange_Centralized(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	seedGatewayPort(t, m, "vpc-1", "aa:bb:cc:00:00:01")
	var barrierCalls int
	var primed []EIPSpec
	nm, err := NewNATManager(m, NATModeCentralized,
		WithFlowsBarrier(func() error { barrierCalls++; return nil }),
		WithNeighPrimer(func(eip EIPSpec) error { primed = append(primed, eip); return nil }))
	require.NoError(t, err)

	base := EIPSpec{VPCID: "vpc-1", ExternalIP: "192.168.1.147", LogicalIP: "172.31.0.4"}
	old := base
	old.PortName = "port-eni-old"
	require.NoError(t, nm.AddEIP(ctx, old))
	require.Equal(t, 1, barrierCalls)
	firstUUID := findNAT(m, "dnat_and_snat", base.LogicalIP).UUID

	// Successor reuses BOTH IPs on a new ENI.
	newer := base
	newer.PortName = "port-eni-new"
	require.NoError(t, nm.AddEIP(ctx, newer))

	got := findNAT(m, "dnat_and_snat", base.LogicalIP)
	require.NotNil(t, got)
	assert.Equal(t, "port-eni-new", got.ExternalIDs["spinifex:logical_port"],
		"row must be re-pointed to the new owning ENI")
	assert.NotEqual(t, firstUUID, got.UUID, "owner change must replace the row, not skip")
	assert.Equal(t, 2, barrierCalls, "re-point must fire the flows barrier (not an idempotent skip)")
	assert.Len(t, primed, 2, "re-point must re-prime reachability")
}

// The genuine idempotent case — same ENI, same external+logical IP — must still
// skip the delete-then-add churn in centralised mode.
func TestNATManager_AddEIP_IdempotentSkip_SameOwner_Centralized(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	seedGatewayPort(t, m, "vpc-1", "aa:bb:cc:00:00:01")
	var barrierCalls int
	nm, err := NewNATManager(m, NATModeCentralized,
		WithFlowsBarrier(func() error { barrierCalls++; return nil }))
	require.NoError(t, err)

	spec := EIPSpec{VPCID: "vpc-1", ExternalIP: "192.168.1.147", LogicalIP: "172.31.0.4", PortName: "port-eni-same"}
	require.NoError(t, nm.AddEIP(ctx, spec))
	require.Equal(t, 1, barrierCalls)
	firstUUID := findNAT(m, "dnat_and_snat", spec.LogicalIP).UUID

	require.NoError(t, nm.AddEIP(ctx, spec))
	assert.Equal(t, firstUUID, findNAT(m, "dnat_and_snat", spec.LogicalIP).UUID,
		"same-ENI re-add must reuse the existing row")
	assert.Equal(t, 1, barrierCalls, "same-ENI re-add must not fire the barrier")
}

func TestNATManager_PruneOrphanEIPs(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-live")
	seedRouter(t, m, "vpc-dead")
	var flushed []string
	nm, err := NewNATManager(m, NATModeDistributed,
		WithNeighFlusher(func(ip string) error { flushed = append(flushed, ip); return nil }))
	require.NoError(t, err)

	require.NoError(t, nm.AddEIP(ctx, EIPSpec{
		VPCID: "vpc-live", ExternalIP: "192.168.1.10", LogicalIP: "172.31.0.5",
		PortName: "port-eni-live", MAC: "aa:aa:aa:aa:aa:aa",
	}))
	require.NoError(t, nm.AddEIP(ctx, EIPSpec{
		VPCID: "vpc-dead", ExternalIP: "192.168.1.11", LogicalIP: "172.31.0.4",
		PortName: "port-eni-orphan", MAC: "bb:bb:bb:bb:bb:bb",
	}))
	// A legacy row with no stamped logical_port must survive (owner undeterminable).
	require.NoError(t, m.AddNAT(ctx, topology.VPCRouter("vpc-dead"), &nbdb.NAT{
		Type: "dnat_and_snat", ExternalIP: "192.168.1.12", LogicalIP: "172.31.0.9",
		ExternalIDs: map[string]string{"spinifex:vpc_id": "vpc-dead"},
	}))
	// A snat row with an orphan stamp must survive (type filter excludes it).
	require.NoError(t, m.AddNAT(ctx, topology.VPCRouter("vpc-dead"), &nbdb.NAT{
		Type: "snat", ExternalIP: "192.168.1.13", LogicalIP: "172.31.0.0/24",
		ExternalIDs: map[string]string{"spinifex:logical_port": "port-eni-gone"},
	}))

	live := map[string]struct{}{"port-eni-live": {}}
	pruned, err := nm.PruneOrphanEIPs(ctx, live)
	require.NoError(t, err)
	assert.Equal(t, 1, pruned, "exactly the stamped orphan dnat_and_snat must be pruned")

	assert.NotNil(t, findNAT(m, "dnat_and_snat", "172.31.0.5"), "live EIP row must survive")
	assert.Nil(t, findNAT(m, "dnat_and_snat", "172.31.0.4"), "orphan EIP row must be pruned")
	assert.NotNil(t, findNAT(m, "dnat_and_snat", "172.31.0.9"), "unstamped legacy row must survive")
	assert.NotNil(t, findNAT(m, "snat", "172.31.0.0/24"), "snat row must survive the dnat_and_snat prune")
	assert.Contains(t, flushed, "192.168.1.11", "orphan external IP ARP must be flushed on prune")
}

func TestNATManager_DeleteEIP_IdempotentOnMissing(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	nm, _ := NewNATManager(m, NATModeDistributed)

	// No prior AddEIP — delete should still succeed.
	require.NoError(t, nm.DeleteEIP(ctx, "vpc-1", "1.2.3.4", "10.0.0.5", ""))
}

func TestNATManager_AddNATGateway_FlowsBarrier_Fires(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	var calls int
	nm, err := NewNATManager(m, NATModeCentralized, WithFlowsBarrier(func() error {
		calls++
		return nil
	}))
	require.NoError(t, err)

	require.NoError(t, nm.AddNATGateway(ctx, NATGWSpec{
		VPCID:        "vpc-1",
		NATGatewayID: "nat-xyz",
		PublicIP:     "9.9.9.9",
		SubnetCIDR:   "10.0.1.0/24",
	}))
	assert.Equal(t, 1, calls, "FlowsBarrier must fire once per AddNATGateway")
}

// TestNATManager_AddNATGateway_IdempotentSkip guards the SNAT-leak root cause:
// the 5-minute reconcile re-publishes the same NAT GW spec, and an unconditional
// create would mint a second identical snat row that survives teardown.
func TestNATManager_AddNATGateway_IdempotentSkip(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	var barrierCalls int
	nm, err := NewNATManager(m, NATModeCentralized, WithFlowsBarrier(func() error {
		barrierCalls++
		return nil
	}))
	require.NoError(t, err)

	spec := NATGWSpec{
		VPCID: "vpc-1", NATGatewayID: "nat-xyz", PublicIP: "9.9.9.9", SubnetCIDR: "172.31.16.0/20",
	}
	require.NoError(t, nm.AddNATGateway(ctx, spec))
	require.Equal(t, 1, barrierCalls, "first AddNATGateway must fire barrier")
	firstUUID := findNAT(m, "snat", spec.SubnetCIDR).UUID

	// Reconcile re-publish: the guard must skip the create.
	require.NoError(t, nm.AddNATGateway(ctx, spec))
	assert.Equal(t, 1, countNAT(m, "snat", spec.SubnetCIDR),
		"idempotent re-add must not mint a duplicate snat row")
	assert.Equal(t, firstUUID, findNAT(m, "snat", spec.SubnetCIDR).UUID,
		"idempotent re-add must reuse the existing row")
	assert.Equal(t, 1, barrierCalls, "FlowsBarrier must not fire on idempotent skip")
}

// TestNATManager_AddNATGateway_IdempotentSkip_MultiSubnet guards the multi-subnet
// topology: one NAT GW public IP serves several private subnets, so its snat rows
// share an external IP but differ by subnet CIDR. The guard keys on (router, subnet
// CIDR) so it dedups per subnet — keying on the public IP alone would match the
// wrong row and let the reconcile mint duplicates.
func TestNATManager_AddNATGateway_IdempotentSkip_MultiSubnet(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	nm, err := NewNATManager(m, NATModeCentralized)
	require.NoError(t, err)

	a := NATGWSpec{VPCID: "vpc-1", NATGatewayID: "nat-xyz", PublicIP: "9.9.9.9", SubnetCIDR: "172.31.16.0/20"}
	b := NATGWSpec{VPCID: "vpc-1", NATGatewayID: "nat-xyz", PublicIP: "9.9.9.9", SubnetCIDR: "172.31.32.0/20"}
	require.NoError(t, nm.AddNATGateway(ctx, a))
	require.NoError(t, nm.AddNATGateway(ctx, b))
	require.Equal(t, 1, countNAT(m, "snat", a.SubnetCIDR))
	require.Equal(t, 1, countNAT(m, "snat", b.SubnetCIDR))

	// Reconcile re-publishes both specs; each must be a per-subnet no-op.
	require.NoError(t, nm.AddNATGateway(ctx, a))
	require.NoError(t, nm.AddNATGateway(ctx, b))
	assert.Equal(t, 1, countNAT(m, "snat", a.SubnetCIDR),
		"re-add of subnet A must not mint a duplicate")
	assert.Equal(t, 1, countNAT(m, "snat", b.SubnetCIDR),
		"re-add of subnet B must not mint a duplicate")
}

// TestNATManager_AddNATGateway_ReplacesStaleOnPublicIPChange guards the EIP-change
// edge: a dropped delete then a recreate with a new EIP for the same subnet must not
// leave the old snat row behind to leak egress via the stale public IP.
func TestNATManager_AddNATGateway_ReplacesStaleOnPublicIPChange(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	nm, err := NewNATManager(m, NATModeCentralized)
	require.NoError(t, err)

	old := NATGWSpec{VPCID: "vpc-1", NATGatewayID: "nat-old", PublicIP: "9.9.9.9", SubnetCIDR: "172.31.16.0/20"}
	require.NoError(t, nm.AddNATGateway(ctx, old))

	// Recreate the subnet's NAT GW with a different EIP; the stale row must go.
	fresh := NATGWSpec{VPCID: "vpc-1", NATGatewayID: "nat-new", PublicIP: "8.8.8.8", SubnetCIDR: "172.31.16.0/20"}
	require.NoError(t, nm.AddNATGateway(ctx, fresh))

	assert.Equal(t, 1, countNAT(m, "snat", fresh.SubnetCIDR),
		"public-IP change must replace, not stack, the snat row")
	got := findNAT(m, "snat", fresh.SubnetCIDR)
	require.NotNil(t, got)
	assert.Equal(t, "8.8.8.8", got.ExternalIP, "surviving row must carry the new public IP")
}

// TestNATManager_DeleteNATGateway_RemovesDuplicates is defensive: any duplicate
// snat row that slipped past the idempotency guard (race, pre-existing) must be
// fully removed on teardown, not left to leak egress past the first delete.
func TestNATManager_DeleteNATGateway_RemovesDuplicates(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	router := seedRouter(t, m, "vpc-1")
	nm, err := NewNATManager(m, NATModeCentralized)
	require.NoError(t, err)

	for range 2 {
		require.NoError(t, m.AddNAT(ctx, router, &nbdb.NAT{
			Type: "snat", ExternalIP: "9.9.9.9", LogicalIP: "172.31.16.0/20",
		}))
	}
	require.Equal(t, 2, countNAT(m, "snat", "172.31.16.0/20"))

	require.NoError(t, nm.DeleteNATGateway(ctx, "vpc-1", "172.31.16.0/20"))
	assert.Equal(t, 0, countNAT(m, "snat", "172.31.16.0/20"),
		"DeleteNATGateway must remove every matching snat row, not just the first")
}

func TestNATManager_AddNATGateway_AndDelete(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	nm, _ := NewNATManager(m, NATModeCentralized)

	require.NoError(t, nm.AddNATGateway(ctx, NATGWSpec{
		VPCID:        "vpc-1",
		NATGatewayID: "nat-xyz",
		PublicIP:     "9.9.9.9",
		SubnetCIDR:   "10.0.1.0/24",
	}))
	got := findNAT(m, "snat", "10.0.1.0/24")
	require.NotNil(t, got)
	assert.Equal(t, "9.9.9.9", got.ExternalIP)
	assert.Equal(t, "nat-xyz", got.ExternalIDs["spinifex:nat_gateway_id"])

	require.NoError(t, nm.DeleteNATGateway(ctx, "vpc-1", "10.0.1.0/24"))
	assert.Nil(t, findNAT(m, "snat", "10.0.1.0/24"))

	// Idempotent re-delete.
	require.NoError(t, nm.DeleteNATGateway(ctx, "vpc-1", "10.0.1.0/24"))
}

// TestNATManager_DeleteNATGateway_CrossRouterIsolation guards against deleting
// a NAT row on router B when DeleteNATGateway targets router A. AWS allows
// overlapping subnet CIDRs across VPCs, so (snat, logicalIP) is not globally unique.
func TestNATManager_DeleteNATGateway_CrossRouterIsolation(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-a")
	seedRouter(t, m, "vpc-b")
	nm, err := NewNATManager(m, NATModeCentralized)
	require.NoError(t, err)

	// Same subnet CIDR routed via NATGW in two different VPCs.
	require.NoError(t, nm.AddNATGateway(ctx, NATGWSpec{
		VPCID: "vpc-a", NATGatewayID: "nat-a", PublicIP: "9.9.9.9", SubnetCIDR: "172.31.0.0/20",
	}))
	require.NoError(t, nm.AddNATGateway(ctx, NATGWSpec{
		VPCID: "vpc-b", NATGatewayID: "nat-b", PublicIP: "8.8.8.8", SubnetCIDR: "172.31.0.0/20",
	}))

	require.NoError(t, nm.DeleteNATGateway(ctx, "vpc-a", "172.31.0.0/20"))

	// vpc-b's snat must remain — only vpc-a's was deleted.
	var survived *nbdb.NAT
	for _, n := range m.NATs {
		if n.Type == "snat" && n.LogicalIP == "172.31.0.0/20" {
			survived = n
		}
	}
	require.NotNil(t, survived, "vpc-b snat row must remain after vpc-a delete")
	assert.Equal(t, "8.8.8.8", survived.ExternalIP, "surviving row must belong to vpc-b")
	assert.Equal(t, "nat-b", survived.ExternalIDs["spinifex:nat_gateway_id"])
}

// TestNATManager_DeleteEIP_CrossRouterIsolation is the dnat_and_snat analogue:
// two VPCs with overlapping private IPs — DeleteEIP on vpc-a must not touch vpc-b.
func TestNATManager_DeleteEIP_CrossRouterIsolation(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-a")
	seedRouter(t, m, "vpc-b")
	nm, err := NewNATManager(m, NATModeDistributed)
	require.NoError(t, err)

	require.NoError(t, nm.AddEIP(ctx, EIPSpec{
		VPCID: "vpc-a", ExternalIP: "1.1.1.1", LogicalIP: "10.0.0.5",
		PortName: "port-a", MAC: "aa:aa:aa:aa:aa:aa",
	}))
	require.NoError(t, nm.AddEIP(ctx, EIPSpec{
		VPCID: "vpc-b", ExternalIP: "2.2.2.2", LogicalIP: "10.0.0.5",
		PortName: "port-b", MAC: "bb:bb:bb:bb:bb:bb",
	}))

	require.NoError(t, nm.DeleteEIP(ctx, "vpc-a", "1.1.1.1", "10.0.0.5", ""))

	var survived *nbdb.NAT
	for _, n := range m.NATs {
		if n.Type == "dnat_and_snat" && n.LogicalIP == "10.0.0.5" {
			survived = n
		}
	}
	require.NotNil(t, survived, "vpc-b dnat_and_snat row must remain after vpc-a delete")
	assert.Equal(t, "2.2.2.2", survived.ExternalIP, "surviving row must belong to vpc-b")
}

// TestNATManager_DeleteEIP_RecycledLogicalIP_NoClobber guards the EIP-recycling
// clobber: a dnat_and_snat row's identity is its external IP, not its logical IP.
// Private IPs are reused as instances recycle, so a stale or retried delete keyed
// on a freed EIP must not tear down whichever live EIP now holds the same private IP.
func TestNATManager_DeleteEIP_RecycledLogicalIP_NoClobber(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	nm, err := NewNATManager(m, NATModeCentralized)
	require.NoError(t, err)

	// Live EIP .174 bound to recycled private IP 172.31.0.4.
	require.NoError(t, nm.AddEIP(ctx, EIPSpec{
		VPCID: "vpc-1", ExternalIP: "192.168.0.174", LogicalIP: "172.31.0.4",
	}))

	// Stale delete for an earlier EIP (.172) on the same recycled private IP.
	require.NoError(t, nm.DeleteEIP(ctx, "vpc-1", "192.168.0.172", "172.31.0.4", ""))

	got := findNAT(m, "dnat_and_snat", "172.31.0.4")
	require.NotNil(t, got, "live EIP row must survive a stale delete for a recycled private IP")
	assert.Equal(t, "192.168.0.174", got.ExternalIP, "the surviving row must be the live EIP")
}

// TestNATManager_DeleteEIP_RecycledExternalIP_NoClobber guards the proven flake:
// a public IP is recycled from a terminated instance to a live one, then the
// dead instance's delayed GC teardown publishes vpc.delete-nat for that external
// IP carrying the OLD logical IP. The delete must be a no-op — deleting by
// external IP alone would tear down the live owner's NAT (and ARP).
func TestNATManager_DeleteEIP_RecycledExternalIP_NoClobber(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	var flushed []string
	nm, err := NewNATManager(m, NATModeCentralized,
		WithNeighFlusher(func(externalIP string) error {
			flushed = append(flushed, externalIP)
			return nil
		}))
	require.NoError(t, err)

	// Live instance now owns 192.168.0.201 → 172.31.0.5.
	require.NoError(t, nm.AddEIP(ctx, EIPSpec{
		VPCID: "vpc-1", ExternalIP: "192.168.0.201", LogicalIP: "172.31.0.5",
	}))
	flushed = nil

	// Stale GC teardown of the dead prior owner (172.31.0.4) of the same IP.
	require.NoError(t, nm.DeleteEIP(ctx, "vpc-1", "192.168.0.201", "172.31.0.4", ""))

	got := findNAT(m, "dnat_and_snat", "172.31.0.5")
	require.NotNil(t, got, "live owner's row must survive a stale delete for the recycled external IP")
	assert.Equal(t, "192.168.0.201", got.ExternalIP)
	assert.Empty(t, flushed, "stale delete must not flush the live owner's host ARP")
}

// TestNATManager_DeleteEIP_RecycledIdenticalPair_OwnerScoped guards the residual
// race left by the (external_ip, logical_ip) pair-key: a terminated instance and
// a live one hold the IDENTICAL public+private pair in the same VPC, so the pair
// matches and the logical-IP guard falls through. The stamped logical port is the
// only discriminator — a stale delete carrying the dead instance's port must be a
// no-op, while a delete for the live owner's own port still removes the row.
func TestNATManager_DeleteEIP_RecycledIdenticalPair_OwnerScoped(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	var flushed []string
	nm, err := NewNATManager(m, NATModeCentralized,
		WithNeighFlusher(func(externalIP string) error {
			flushed = append(flushed, externalIP)
			return nil
		}))
	require.NoError(t, err)

	// Live owner: 192.168.1.93 → 172.31.0.4 on the live ENI's port.
	require.NoError(t, nm.AddEIP(ctx, EIPSpec{
		VPCID: "vpc-1", ExternalIP: "192.168.1.93", LogicalIP: "172.31.0.4",
		PortName: "port-live",
	}))
	flushed = nil

	// Stale GC teardown of the terminated owner: identical pair, dead ENI port.
	require.NoError(t, nm.DeleteEIP(ctx, "vpc-1", "192.168.1.93", "172.31.0.4", "port-dead"))
	got := findNAT(m, "dnat_and_snat", "172.31.0.4")
	require.NotNil(t, got, "live owner's row must survive a stale delete for the recycled identical pair")
	assert.Equal(t, "192.168.1.93", got.ExternalIP)
	assert.Empty(t, flushed, "stale identical-pair delete must not flush the live owner's host ARP")

	// The live owner's own delete (matching port) still removes the row.
	require.NoError(t, nm.DeleteEIP(ctx, "vpc-1", "192.168.1.93", "172.31.0.4", "port-live"))
	assert.Nil(t, findNAT(m, "dnat_and_snat", "172.31.0.4"),
		"a delete carrying the owning port must remove the row")
}

func TestNATManager_AddSNAT_AndDelete(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	nm, _ := NewNATManager(m, NATModeDistributed)

	require.NoError(t, nm.AddSNAT(ctx, "vpc-1", "10.0.0.0/16", "1.2.3.4"))
	got := findNAT(m, "snat", "10.0.0.0/16")
	require.NotNil(t, got)
	assert.Equal(t, "1.2.3.4", got.ExternalIP)
	assert.Equal(t, "igw-default-snat", got.ExternalIDs["spinifex:role"])

	require.NoError(t, nm.DeleteSNAT(ctx, "vpc-1", "10.0.0.0/16"))
	assert.Nil(t, findNAT(m, "snat", "10.0.0.0/16"))
	require.NoError(t, nm.DeleteSNAT(ctx, "vpc-1", "10.0.0.0/16"))
}

func TestNATManager_AddSystemInstanceSNAT_AndDelete(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	nm, _ := NewNATManager(m, NATModeDistributed)

	require.NoError(t, nm.AddSystemInstanceSNAT(ctx, "vpc-1", "172.31.4.10/32", "1.2.3.4"))
	got := findNAT(m, "snat", "172.31.4.10/32")
	require.NotNil(t, got)
	assert.Equal(t, "1.2.3.4", got.ExternalIP)
	assert.Equal(t, "system-instance-egress", got.ExternalIDs["spinifex:role"])
	// snat-only: no dnat_and_snat row, so the instance stays unreachable inbound.
	assert.Nil(t, findNAT(m, "dnat_and_snat", "172.31.4.10/32"))

	require.NoError(t, nm.DeleteSystemInstanceSNAT(ctx, "vpc-1", "172.31.4.10/32"))
	assert.Nil(t, findNAT(m, "snat", "172.31.4.10/32"))
	// Idempotent on a missing rule.
	require.NoError(t, nm.DeleteSystemInstanceSNAT(ctx, "vpc-1", "172.31.4.10/32"))
}

func TestNATManager_AddSNAT_ExemptSetAndIdempotency(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	nm, err := NewNATManager(m, NATModeRouted,
		WithSNATExemptSet("spinifex_nat_exempt", []string{"100.127.0.0/24", "192.168.50.0/24"}))
	require.NoError(t, err)

	// Fresh add: row created with exempt ref, set contains configured CIDRs.
	require.NoError(t, nm.AddSNAT(ctx, "vpc-1", "10.0.0.0/16", "100.127.0.10"))
	nat := findNAT(m, "snat", "10.0.0.0/16")
	require.NotNil(t, nat)
	require.NotNil(t, nat.ExemptedExtIps)
	as, err := m.GetAddressSet(ctx, "spinifex_nat_exempt")
	require.NoError(t, err)
	assert.Equal(t, as.UUID, *nat.ExemptedExtIps)
	assert.Equal(t, []string{"100.127.0.0/24", "192.168.50.0/24"}, as.Addresses)

	// Double add: idempotent skip, exactly one row, same UUID.
	firstUUID := nat.UUID
	require.NoError(t, nm.AddSNAT(ctx, "vpc-1", "10.0.0.0/16", "100.127.0.10"))
	assert.Equal(t, 1, countNAT(m, "snat", "10.0.0.0/16"))
	assert.Equal(t, firstUUID, findNAT(m, "snat", "10.0.0.0/16").UUID)

	// External IP change: stale row replaced, still one row.
	require.NoError(t, nm.AddSNAT(ctx, "vpc-1", "10.0.0.0/16", "100.127.0.20"))
	assert.Equal(t, 1, countNAT(m, "snat", "10.0.0.0/16"))
	assert.Equal(t, "100.127.0.20", findNAT(m, "snat", "10.0.0.0/16").ExternalIP)
}

func TestNATManager_AddSNAT_PatchesLegacyRow(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	router := seedRouter(t, m, "vpc-1")

	// Legacy row minted before the exempt option existed (no ref).
	require.NoError(t, m.AddNAT(ctx, router, &nbdb.NAT{
		Type: "snat", ExternalIP: "100.127.0.10", LogicalIP: "10.0.0.0/16",
	}))

	nm, err := NewNATManager(m, NATModeRouted,
		WithSNATExemptSet("spinifex_nat_exempt", []string{"100.127.0.0/24"}))
	require.NoError(t, err)
	require.NoError(t, nm.AddSNAT(ctx, "vpc-1", "10.0.0.0/16", "100.127.0.10"))

	nat := findNAT(m, "snat", "10.0.0.0/16")
	require.NotNil(t, nat.ExemptedExtIps, "legacy row must be patched in place")
	as, err := m.GetAddressSet(ctx, "spinifex_nat_exempt")
	require.NoError(t, err)
	assert.Equal(t, as.UUID, *nat.ExemptedExtIps)
	assert.Equal(t, 1, countNAT(m, "snat", "10.0.0.0/16"), "patch must not mint a second row")
}

func TestNATManager_AddSNAT_NoExemptOutsideRoutedMode(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")

	// Option set but mode centralised: no set created, no ref stamped.
	nm, err := NewNATManager(m, NATModeCentralized,
		WithSNATExemptSet("spinifex_nat_exempt", []string{"100.127.0.0/24"}))
	require.NoError(t, err)
	require.NoError(t, nm.AddSNAT(ctx, "vpc-1", "10.0.0.0/16", "100.127.0.10"))

	nat := findNAT(m, "snat", "10.0.0.0/16")
	require.NotNil(t, nat)
	assert.Nil(t, nat.ExemptedExtIps)
	assert.Empty(t, m.AddressSets)
}

func TestNATManager_AddEIP_RoutedStampsExemptSet(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	nm, err := NewNATManager(m, NATModeRouted,
		WithSNATExemptSet("spinifex_nat_exempt", []string{"100.127.0.0/24"}))
	require.NoError(t, err)

	spec := EIPSpec{VPCID: "vpc-1", ExternalIP: "192.168.50.80", LogicalIP: "10.0.0.5"}
	require.NoError(t, nm.AddEIP(ctx, spec))

	nat := findNAT(m, "dnat_and_snat", "10.0.0.5")
	require.NotNil(t, nat)
	require.NotNil(t, nat.ExemptedExtIps)
	as, err := m.GetAddressSet(ctx, "spinifex_nat_exempt")
	require.NoError(t, err)
	assert.Equal(t, as.UUID, *nat.ExemptedExtIps)
}

func TestNATManager_AddEIP_IdempotentSkipPatchesExempt(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	router := seedRouter(t, m, "vpc-1")

	// Pre-existing routed EIP row without the ref (pre-upgrade).
	require.NoError(t, m.AddNAT(ctx, router, &nbdb.NAT{
		Type: "dnat_and_snat", ExternalIP: "192.168.50.80", LogicalIP: "10.0.0.5",
		ExternalIDs: map[string]string{"spinifex:vpc_id": "vpc-1", "spinifex:public_ip": "192.168.50.80"},
	}))

	nm, err := NewNATManager(m, NATModeRouted,
		WithSNATExemptSet("spinifex_nat_exempt", []string{"100.127.0.0/24"}))
	require.NoError(t, err)
	spec := EIPSpec{VPCID: "vpc-1", ExternalIP: "192.168.50.80", LogicalIP: "10.0.0.5"}
	require.NoError(t, nm.AddEIP(ctx, spec))

	nat := findNAT(m, "dnat_and_snat", "10.0.0.5")
	require.NotNil(t, nat.ExemptedExtIps, "skip path must patch the exempt ref in place")
	assert.Equal(t, 1, countNAT(m, "dnat_and_snat", "10.0.0.5"))
}

func TestNATManager_AddEIP_NoExemptInDistributedMode(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	nm, err := NewNATManager(m, NATModeDistributed,
		WithSNATExemptSet("spinifex_nat_exempt", []string{"100.127.0.0/24"}))
	require.NoError(t, err)

	require.NoError(t, nm.AddEIP(ctx, EIPSpec{
		VPCID: "vpc-1", ExternalIP: "1.2.3.4", LogicalIP: "10.0.0.5",
		PortName: "port-eni-abc", MAC: "aa:bb:cc:dd:ee:ff",
	}))
	nat := findNAT(m, "dnat_and_snat", "10.0.0.5")
	require.NotNil(t, nat)
	assert.Nil(t, nat.ExemptedExtIps)
	assert.Empty(t, m.AddressSets)
}
