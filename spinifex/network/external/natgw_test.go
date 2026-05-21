package external

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
	"github.com/mulgadc/spinifex/spinifex/services/vpcd/nbdb"
)

func TestNewNATGWManager_RejectsNilNAT(t *testing.T) {
	_, err := NewNATGWManager(nil)
	require.Error(t, err)
}

func TestNATGWManager_AttachAndDetach(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	require.NoError(t, m.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{Name: topology.VPCRouter("vpc-1")}))
	nm, _ := policy.NewNATManager(m, policy.NATModeCentralized)
	mgr, err := NewNATGWManager(nm)
	require.NoError(t, err)

	require.NoError(t, mgr.AttachNATGateway(ctx, policy.NATGWSpec{
		VPCID: "vpc-1", NATGatewayID: "nat-xyz",
		PublicIP: "9.9.9.9", SubnetCIDR: "10.0.1.0/24",
	}))

	var found *nbdb.NAT
	for _, n := range m.NATs {
		if n.Type == "snat" && n.LogicalIP == "10.0.1.0/24" {
			found = n
		}
	}
	require.NotNil(t, found)
	assert.Equal(t, "9.9.9.9", found.ExternalIP)

	require.NoError(t, mgr.DetachNATGateway(ctx, "vpc-1", "10.0.1.0/24"))
	require.NoError(t, mgr.DetachNATGateway(ctx, "vpc-1", "10.0.1.0/24")) // idempotent
}

func TestNATGWManager_AttachRejectsEmptyFields(t *testing.T) {
	nm, _ := policy.NewNATManager(mock.New(), policy.NATModeCentralized)
	mgr, _ := NewNATGWManager(nm)
	require.Error(t, mgr.AttachNATGateway(context.Background(), policy.NATGWSpec{}))
}
