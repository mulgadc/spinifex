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

func TestNATModeFromUplinkMode(t *testing.T) {
	assert.Equal(t, NATModeDistributed, NATModeFromUplinkMode(host.UplinkModePhysical))
	assert.Equal(t, NATModeCentralized, NATModeFromUplinkMode(host.UplinkModeVeth))
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

func TestNATManager_AddEIP_GARP_FiresDistributed(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	var got []EIPSpec
	nm, err := NewNATManager(m, NATModeDistributed, WithGARPEmitter(func(spec EIPSpec) error {
		got = append(got, spec)
		return nil
	}))
	require.NoError(t, err)

	spec := EIPSpec{
		VPCID: "vpc-1", ExternalIP: "1.2.3.4", LogicalIP: "10.0.0.5",
		PortName: "port-eni-abc", MAC: "aa:bb:cc:dd:ee:ff",
	}
	require.NoError(t, nm.AddEIP(ctx, spec))
	require.Len(t, got, 1, "GARPEmitter must fire once per AddEIP")
	assert.Equal(t, spec, got[0], "emitter must receive the full EIPSpec so it can derive the chassisredirect port")
}

func TestNATManager_AddEIP_GARP_FiresCentralized(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	var got []EIPSpec
	nm, err := NewNATManager(m, NATModeCentralized, WithGARPEmitter(func(spec EIPSpec) error {
		got = append(got, spec)
		return nil
	}))
	require.NoError(t, err)

	spec := EIPSpec{VPCID: "vpc-1", ExternalIP: "1.2.3.4", LogicalIP: "10.0.0.5"}
	require.NoError(t, nm.AddEIP(ctx, spec))
	require.Len(t, got, 1, "GARPEmitter must fire in centralized mode too")
	assert.Empty(t, got[0].PortName, "centralized AddEIP carries no LSP — emitter must derive cr-gw-<vpc> itself")
}

func TestNATManager_AddEIP_GARP_FailureNonFatal(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	nm, err := NewNATManager(m, NATModeDistributed, WithGARPEmitter(func(EIPSpec) error {
		return assert.AnError
	}))
	require.NoError(t, err)

	require.NoError(t, nm.AddEIP(ctx, EIPSpec{
		VPCID: "vpc-1", ExternalIP: "1.2.3.4", LogicalIP: "10.0.0.5",
	}), "GARP emitter failure must not propagate")
	assert.NotNil(t, findNAT(m, "dnat_and_snat", "10.0.0.5"), "NAT row must persist despite GARP failure")
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

func TestNATManager_DeleteEIP_IdempotentOnMissing(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	nm, _ := NewNATManager(m, NATModeDistributed)

	// No prior AddEIP — delete should still succeed.
	require.NoError(t, nm.DeleteEIP(ctx, "vpc-1", "10.0.0.5"))
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

// TestNATManager_DeleteNATGateway_CrossRouterIsolation guards against a NAT
// row owned by router B being mutated/deleted when DeleteNATGateway is called
// on router A and both routers carry the same subnet CIDR. AWS allows subnet
// CIDRs to repeat per-VPC, so (snat, logicalIP) is not globally unique. Bug
// observed in CI Phase 8d: shared 172.31.x CIDRs across VPCs caused
// DeleteEIP/DeleteNAT to hit the wrong NAT row, leaving orphaned rules that
// corrupted ARP/conntrack for the surviving NATGW EIP.
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

// TestNATManager_DeleteEIP_CrossRouterIsolation is the dnat_and_snat analogue
// of the cross-router NATGW isolation test. Two VPCs each with a private VM
// at 10.0.0.5 (legal — VPC CIDRs may overlap). DeleteEIP on vpc-a must not
// touch vpc-b's NAT row.
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

	require.NoError(t, nm.DeleteEIP(ctx, "vpc-a", "10.0.0.5"))

	var survived *nbdb.NAT
	for _, n := range m.NATs {
		if n.Type == "dnat_and_snat" && n.LogicalIP == "10.0.0.5" {
			survived = n
		}
	}
	require.NotNil(t, survived, "vpc-b dnat_and_snat row must remain after vpc-a delete")
	assert.Equal(t, "2.2.2.2", survived.ExternalIP, "surviving row must belong to vpc-b")
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
