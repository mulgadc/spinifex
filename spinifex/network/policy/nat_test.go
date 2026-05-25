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
