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
