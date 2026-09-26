package policy

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

type recordedBinder struct {
	binds   []string
	unbinds []string
	bindErr error
}

func (b *recordedBinder) hooks() HostEIPBinder {
	return HostEIPBinder{
		Bind: func(eip EIPSpec, gwLrpIP string) error {
			b.binds = append(b.binds, eip.ExternalIP+" via "+gwLrpIP)
			return b.bindErr
		},
		Unbind: func(externalIP string) error {
			b.unbinds = append(b.unbinds, externalIP)
			return nil
		},
	}
}

func seedGatewayPortIP(t *testing.T, m *mock.Client, vpcID, gwIP string, networks []string) {
	t.Helper()
	extIDs := map[string]string{}
	if gwIP != "" {
		extIDs[GatewayIPExtIDKey] = gwIP
	}
	require.NoError(t, m.CreateLogicalRouterPort(context.Background(),
		topology.VPCRouter(vpcID),
		&nbdb.LogicalRouterPort{
			Name:        topology.GatewayRouterPort(vpcID),
			MAC:         "02:00:00:00:00:01",
			Networks:    networks,
			ExternalIDs: extIDs,
		}))
}

func TestNATManager_AddEIP_Routed_BindsHostEIP(t *testing.T) {
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	seedGatewayPortIP(t, m, "vpc-1", "100.127.0.10", nil)
	b := &recordedBinder{}
	mgr, err := NewNATManager(m, NATModeRouted, WithHostEIPBinder(b.hooks()))
	require.NoError(t, err)

	require.NoError(t, mgr.AddEIP(context.Background(), EIPSpec{
		VPCID: "vpc-1", ExternalIP: "192.168.1.200", LogicalIP: "10.0.1.5",
	}))
	require.Len(t, b.binds, 1)
	assert.Equal(t, "192.168.1.200 via 100.127.0.10", b.binds[0])
}

func TestNATManager_AddEIP_Routed_BindsOnIdempotentSkip(t *testing.T) {
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	seedGatewayPortIP(t, m, "vpc-1", "100.127.0.10", nil)
	b := &recordedBinder{}
	mgr, err := NewNATManager(m, NATModeRouted, WithHostEIPBinder(b.hooks()))
	require.NoError(t, err)

	eip := EIPSpec{VPCID: "vpc-1", ExternalIP: "192.168.1.200", LogicalIP: "10.0.1.5"}
	require.NoError(t, mgr.AddEIP(context.Background(), eip))
	require.NoError(t, mgr.AddEIP(context.Background(), eip))
	assert.Equal(t, 1, countNAT(m, "dnat_and_snat", "10.0.1.5"), "second add must not duplicate the row")
	assert.Len(t, b.binds, 2, "host state is volatile; the skip path must re-bind")
}

func TestNATManager_AddEIP_Routed_GatewayIPFallsBackToNetworks(t *testing.T) {
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	seedGatewayPortIP(t, m, "vpc-1", "", []string{"100.127.0.7/24"})
	b := &recordedBinder{}
	mgr, err := NewNATManager(m, NATModeRouted, WithHostEIPBinder(b.hooks()))
	require.NoError(t, err)

	require.NoError(t, mgr.AddEIP(context.Background(), EIPSpec{
		VPCID: "vpc-1", ExternalIP: "192.168.1.200", LogicalIP: "10.0.1.5",
	}))
	require.Len(t, b.binds, 1)
	assert.Equal(t, "192.168.1.200 via 100.127.0.7", b.binds[0])
}

func TestNATManager_AddEIP_Routed_NoGatewayLRPFailsBind(t *testing.T) {
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	b := &recordedBinder{}
	mgr, err := NewNATManager(m, NATModeRouted, WithHostEIPBinder(b.hooks()))
	require.NoError(t, err)

	err = mgr.AddEIP(context.Background(), EIPSpec{
		VPCID: "vpc-1", ExternalIP: "192.168.1.200", LogicalIP: "10.0.1.5",
	})
	require.Error(t, err, "no gateway LRP means the /32 route has no next hop")
	assert.Empty(t, b.binds)
}

func TestNATManager_AddEIP_Routed_BindFailureSurfaces(t *testing.T) {
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	seedGatewayPortIP(t, m, "vpc-1", "100.127.0.10", nil)
	b := &recordedBinder{bindErr: fmt.Errorf("ip route replace: exit 2")}
	mgr, err := NewNATManager(m, NATModeRouted, WithHostEIPBinder(b.hooks()))
	require.NoError(t, err)

	err = mgr.AddEIP(context.Background(), EIPSpec{
		VPCID: "vpc-1", ExternalIP: "192.168.1.200", LogicalIP: "10.0.1.5",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bind host EIP 192.168.1.200")
}

func TestNATManager_DeleteEIP_Routed_UnbindsHostEIP(t *testing.T) {
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	seedGatewayPortIP(t, m, "vpc-1", "100.127.0.10", nil)
	b := &recordedBinder{}
	mgr, err := NewNATManager(m, NATModeRouted, WithHostEIPBinder(b.hooks()))
	require.NoError(t, err)

	eip := EIPSpec{VPCID: "vpc-1", ExternalIP: "192.168.1.200", LogicalIP: "10.0.1.5"}
	require.NoError(t, mgr.AddEIP(context.Background(), eip))
	require.NoError(t, mgr.DeleteEIP(context.Background(), "vpc-1", "192.168.1.200", "10.0.1.5", ""))
	assert.Equal(t, []string{"192.168.1.200"}, b.unbinds)
}

// An absent dnat_and_snat row is not a reassignment: nobody owns the address,
// so the orphaned host plumbing is precisely what the delete has to remove.
// Returning early on it left the /32 route and proxy-ARP entry behind, which
// is what a disassociate looked like from the host.
func TestNATManager_DeleteEIP_Routed_UnbindsWhenRowAlreadyGone(t *testing.T) {
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	seedGatewayPortIP(t, m, "vpc-1", "100.127.0.10", nil)
	b := &recordedBinder{}
	mgr, err := NewNATManager(m, NATModeRouted, WithHostEIPBinder(b.hooks()))
	require.NoError(t, err)

	// No AddEIP: the row never existed, standing in for one already removed.
	require.NoError(t, mgr.DeleteEIP(context.Background(), "vpc-1", "192.168.1.200", "10.0.1.5", "eni-1"))
	assert.Equal(t, []string{"192.168.1.200"}, b.unbinds)
}

// The reassignment cases keep their early return: a different owner holds the
// address, and its host plumbing must survive a late delete for the old one.
func TestNATManager_DeleteEIP_Routed_ReassignedRowKeepsHostPlumbing(t *testing.T) {
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	seedGatewayPortIP(t, m, "vpc-1", "100.127.0.10", nil)
	b := &recordedBinder{}
	mgr, err := NewNATManager(m, NATModeRouted, WithHostEIPBinder(b.hooks()))
	require.NoError(t, err)

	require.NoError(t, mgr.AddEIP(context.Background(), EIPSpec{
		VPCID: "vpc-1", ExternalIP: "192.168.1.200", LogicalIP: "10.0.1.9",
	}))
	b.unbinds = nil

	// Stale delete naming the previous logical IP.
	require.NoError(t, mgr.DeleteEIP(context.Background(), "vpc-1", "192.168.1.200", "10.0.1.5", ""))
	assert.Empty(t, b.unbinds, "a reassigned EIP must keep its host plumbing")
}

func TestNATManager_AddEIP_NonRoutedNeverBinds(t *testing.T) {
	for _, mode := range []NATMode{NATModeDistributed, NATModeCentralized} {
		m := mock.New()
		seedRouter(t, m, "vpc-1")
		seedGatewayPortIP(t, m, "vpc-1", "192.168.1.240", nil)
		b := &recordedBinder{}
		mgr, err := NewNATManager(m, mode, WithHostEIPBinder(b.hooks()))
		require.NoError(t, err)

		require.NoError(t, mgr.AddEIP(context.Background(), EIPSpec{
			VPCID: "vpc-1", ExternalIP: "192.168.1.200", LogicalIP: "10.0.1.5",
		}))
		require.NoError(t, mgr.DeleteEIP(context.Background(), "vpc-1", "192.168.1.200", "10.0.1.5", ""))
		assert.Empty(t, b.binds, "mode %v must not touch host EIP plumbing", mode)
		assert.Empty(t, b.unbinds, "mode %v must not touch host EIP plumbing", mode)
	}
}

// Routed mode distributes an EIP the same way pool mode does, and for the same
// reason: OVN's ARP responder answers for the external IP on the external
// segment using the per-rule external MAC. In routed mode that segment is the
// per-node transit veth, so answering it can only be the node running the
// instance.
func TestNATManager_AddEIP_Routed_DistributedWhenPortAndMACAreKnown(t *testing.T) {
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	seedGatewayPortIP(t, m, "vpc-1", "100.127.0.10", nil)
	b := &recordedBinder{}
	mgr, err := NewNATManager(m, NATModeRouted, WithHostEIPBinder(b.hooks()))
	require.NoError(t, err)

	require.NoError(t, mgr.AddEIP(context.Background(), EIPSpec{
		VPCID: "vpc-1", ExternalIP: "192.168.1.200", LogicalIP: "10.0.1.5",
		PortName: "port-eni-1", MAC: "52:54:00:aa:bb:cc",
	}))

	row := findNAT(m, "dnat_and_snat", "10.0.1.5")
	require.NotNil(t, row)
	require.NotNil(t, row.ExternalMAC, "a distributed rule carries the ENI MAC")
	assert.Equal(t, "52:54:00:aa:bb:cc", *row.ExternalMAC)
	require.NotNil(t, row.LogicalPort, "a distributed rule carries the owning ENI port")
	assert.Equal(t, "port-eni-1", *row.LogicalPort)

	// The bind must name no next hop. A gateway LRP address lives on one
	// chassis's transit veth, so routing through it is exactly what made every
	// non-gateway node's EIPs unreachable.
	require.Len(t, b.binds, 1)
	assert.Equal(t, "192.168.1.200 via ", b.binds[0])
}

// A distributed EIP needs no gateway LRP, so a VPC whose IGW has not yet
// produced one still binds. Before this, the missing address failed the bind on
// every node including the one running the instance.
func TestNATManager_AddEIP_Routed_DistributedBindsWithoutAGatewayLRP(t *testing.T) {
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	b := &recordedBinder{}
	mgr, err := NewNATManager(m, NATModeRouted, WithHostEIPBinder(b.hooks()))
	require.NoError(t, err)

	require.NoError(t, mgr.AddEIP(context.Background(), EIPSpec{
		VPCID: "vpc-1", ExternalIP: "192.168.1.200", LogicalIP: "10.0.1.5",
		PortName: "port-eni-1", MAC: "52:54:00:aa:bb:cc",
	}))
	require.Len(t, b.binds, 1)
	assert.Equal(t, "192.168.1.200 via ", b.binds[0])
}

// An EIP with no MAC — a record written before the ENI was known, or an ENI
// that never reported one — stays centralised and keeps demanding its next hop.
// The two shapes coexist so a cluster part-way through an upgrade is never in a
// state neither side handles.
func TestNATManager_AddEIP_Routed_NoMACStaysCentralised(t *testing.T) {
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	seedGatewayPortIP(t, m, "vpc-1", "100.127.0.10", nil)
	b := &recordedBinder{}
	mgr, err := NewNATManager(m, NATModeRouted, WithHostEIPBinder(b.hooks()))
	require.NoError(t, err)

	require.NoError(t, mgr.AddEIP(context.Background(), EIPSpec{
		VPCID: "vpc-1", ExternalIP: "192.168.1.200", LogicalIP: "10.0.1.5",
		PortName: "port-eni-1",
	}))

	row := findNAT(m, "dnat_and_snat", "10.0.1.5")
	require.NotNil(t, row)
	assert.Nil(t, row.ExternalMAC, "no MAC means no distributed rule")
	require.Len(t, b.binds, 1)
	assert.Equal(t, "192.168.1.200 via 100.127.0.10", b.binds[0])
}

// The constraint this change is held to: distributing the routed path must not
// move pool or static mode. NATModeDistributed and NATModeCentralized are
// asserted here against the same input, so a later edit to the shared predicate
// that widened either of them would fail rather than ship.
func TestNATManager_AddEIP_OtherModesAreUnmoved(t *testing.T) {
	eip := EIPSpec{
		VPCID: "vpc-1", ExternalIP: "192.168.1.200", LogicalIP: "10.0.1.5",
		PortName: "port-eni-1", MAC: "52:54:00:aa:bb:cc",
	}
	for _, tc := range []struct {
		name            string
		mode            NATMode
		wantDistributed bool
	}{
		{"pool and veth uplinks stay distributed", NATModeDistributed, true},
		{"centralised stays centralised", NATModeCentralized, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := mock.New()
			seedRouter(t, m, "vpc-1")
			b := &recordedBinder{}
			mgr, err := NewNATManager(m, tc.mode, WithHostEIPBinder(b.hooks()))
			require.NoError(t, err)
			require.NoError(t, mgr.AddEIP(context.Background(), eip))

			row := findNAT(m, "dnat_and_snat", "10.0.1.5")
			require.NotNil(t, row)
			assert.Equal(t, tc.wantDistributed, row.ExternalMAC != nil,
				"external_mac presence must not change outside routed mode")
			assert.Empty(t, b.binds, "only routed mode plumbs host state")
		})
	}
}

// listingBinder adds List to recordedBinder so the prune half of BindHostEIPs
// can be exercised.
type listingBinder struct {
	recordedBinder

	bound []string
}

func (b *listingBinder) listHooks() HostEIPBinder {
	h := b.hooks()
	h.Unbind = func(externalIP string) error {
		b.unbinds = append(b.unbinds, externalIP)
		return nil
	}
	h.List = func() ([]string, error) { return b.bound, nil }
	return h
}

// Every node has to plumb its own guests. Leader-gating this left the node that
// never won the reconcile lease with no route to the guest it was running,
// which is two of three guests dark on a three-node cluster.
func TestNATManager_BindHostEIPs_BindsEveryIntentEIPAndPrunesTheRest(t *testing.T) {
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	seedGatewayPortIP(t, m, "vpc-1", "100.127.0.10", nil)
	b := &listingBinder{bound: []string{"192.168.1.200", "192.168.1.201", "192.168.1.250"}}
	mgr, err := NewNATManager(m, NATModeRouted, WithHostEIPBinder(b.listHooks()))
	require.NoError(t, err)

	require.NoError(t, mgr.BindHostEIPs(context.Background(), []EIPSpec{
		{VPCID: "vpc-1", ExternalIP: "192.168.1.200", LogicalIP: "10.0.1.5",
			PortName: "port-eni-1", MAC: "52:54:00:aa:bb:cc"},
		{VPCID: "vpc-1", ExternalIP: "192.168.1.201", LogicalIP: "10.0.1.6",
			PortName: "port-eni-2", MAC: "52:54:00:aa:bb:dd"},
	}))

	assert.Equal(t, []string{"192.168.1.200 via ", "192.168.1.201 via "}, b.binds,
		"a distributed EIP binds on-link, with no gateway LRP next hop")
	assert.Equal(t, []string{"192.168.1.250"}, b.unbinds,
		"only the binding intent no longer asks for is swept")
}

// BindHostEIPs writes no NB rows, so it must leave the OVN state it reads
// alone — the leader-gated pass is still the only writer of dnat_and_snat.
func TestNATManager_BindHostEIPs_WritesNoNATRows(t *testing.T) {
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	seedGatewayPortIP(t, m, "vpc-1", "100.127.0.10", nil)
	b := &listingBinder{}
	mgr, err := NewNATManager(m, NATModeRouted, WithHostEIPBinder(b.listHooks()))
	require.NoError(t, err)

	require.NoError(t, mgr.BindHostEIPs(context.Background(), []EIPSpec{
		{VPCID: "vpc-1", ExternalIP: "192.168.1.200", LogicalIP: "10.0.1.5",
			PortName: "port-eni-1", MAC: "52:54:00:aa:bb:cc"},
	}))

	nat, err := m.FindNATByExternalIP(context.Background(), "dnat_and_snat", "192.168.1.200")
	require.NoError(t, err)
	assert.Nil(t, nat, "BindHostEIPs must not create a NAT row")
}

// A resolver failure leaves the wanted set short of an address that is still
// live, so sweeping against it would tear down plumbing a guest is using.
func TestNATManager_BindHostEIPs_SkipsThePruneWhenIntentCouldNotBeResolved(t *testing.T) {
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	seedGatewayPortIP(t, m, "vpc-1", "100.127.0.10", nil)
	b := &listingBinder{bound: []string{"10.200.0.57"}}
	mgr, err := NewNATManager(m, NATModeRouted,
		WithHostEIPBinder(b.listHooks()),
		WithDatapathResolver(func(_ context.Context, _ string) (string, error) {
			return "", fmt.Errorf("OCI: private IP lookup failed")
		}))
	require.NoError(t, err)

	err = mgr.BindHostEIPs(context.Background(), []EIPSpec{
		{VPCID: "vpc-1", ExternalIP: "192.168.1.200", LogicalIP: "10.0.1.5",
			PortName: "port-eni-1", MAC: "52:54:00:aa:bb:cc"},
	})
	require.Error(t, err)
	assert.Empty(t, b.unbinds, "an unresolved intent must not drive a prune")
}

// Pool and static deployments keep the host out of the EIP path entirely, so
// this pass must stay inert there however often it runs.
func TestNATManager_BindHostEIPs_IsInertOutsideRoutedMode(t *testing.T) {
	for _, mode := range []NATMode{NATModeDistributed, NATModeCentralized} {
		t.Run(mode.String(), func(t *testing.T) {
			m := mock.New()
			seedRouter(t, m, "vpc-1")
			b := &listingBinder{bound: []string{"192.168.1.250"}}
			mgr, err := NewNATManager(m, mode, WithHostEIPBinder(b.listHooks()))
			require.NoError(t, err)

			require.NoError(t, mgr.BindHostEIPs(context.Background(), []EIPSpec{
				{VPCID: "vpc-1", ExternalIP: "192.168.1.200", LogicalIP: "10.0.1.5",
					PortName: "port-eni-1", MAC: "52:54:00:aa:bb:cc"},
			}))
			assert.Empty(t, b.binds)
			assert.Empty(t, b.unbinds)
		})
	}
}
