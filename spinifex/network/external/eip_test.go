package external

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

func TestNewEIPManager_RejectsNilNAT(t *testing.T) {
	_, err := NewEIPManager(nil, nil)
	require.Error(t, err)
}

func TestEIPManager_AttachAndDetach(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	require.NoError(t, m.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{Name: topology.VPCRouter("vpc-1")}))
	nm, _ := policy.NewNATManager(m, policy.NATModeDistributed)
	barrierCalls := 0
	mgr, err := NewEIPManager(nm, func() error { barrierCalls++; return nil })
	require.NoError(t, err)

	require.NoError(t, mgr.AttachEIP(ctx, policy.EIPSpec{
		VPCID: "vpc-1", ExternalIP: "1.2.3.4", LogicalIP: "10.0.0.5",
		PortName: "port-a", MAC: "aa:bb:cc:dd:ee:ff",
	}))

	var found *nbdb.NAT
	for _, n := range m.NATs {
		if n.LogicalIP == "10.0.0.5" {
			found = n
		}
	}
	require.NotNil(t, found)
	assert.Equal(t, 1, barrierCalls)

	require.NoError(t, mgr.DetachEIP(ctx, "vpc-1", "1.2.3.4", "10.0.0.5"))
	for _, n := range m.NATs {
		assert.NotEqual(t, "10.0.0.5", n.LogicalIP)
	}
}

func TestEIPManager_AttachRejectsEmptyFields(t *testing.T) {
	nm, _ := policy.NewNATManager(mock.New(), policy.NATModeDistributed)
	mgr, _ := NewEIPManager(nm, nil)
	require.Error(t, mgr.AttachEIP(context.Background(), policy.EIPSpec{}))
}

func TestEIPManager_DetachIdempotent(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	require.NoError(t, m.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{Name: topology.VPCRouter("vpc-1")}))
	nm, _ := policy.NewNATManager(m, policy.NATModeDistributed)
	mgr, _ := NewEIPManager(nm, nil)
	require.NoError(t, mgr.DetachEIP(ctx, "vpc-1", "9.9.9.9", "10.0.0.99"))
}
