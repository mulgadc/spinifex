package external

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	handlers_imds "github.com/mulgadc/spinifex/spinifex/handlers/imds"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/mulgadc/spinifex/spinifex/testutil"
)

func newIMDSManager(t *testing.T, m *mock.Client) (IMDSTopologyManager, *handlers_imds.VethStore) {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	vpcVeth, _, err := handlers_imds.InitBuckets(js, 1)
	require.NoError(t, err)
	store := handlers_imds.NewVethStore(vpcVeth)
	mgr, err := NewIMDSTopologyManager(m, policy.NewRouteManager(m), store)
	require.NoError(t, err)
	return mgr, store
}

func findIMDSRoute(m *mock.Client, prefix string) *nbdb.LogicalRouterStaticRoute {
	for _, r := range m.StaticRoutes {
		if r.IPPrefix == prefix {
			return r
		}
	}
	return nil
}

func TestNewIMDSTopologyManager_RejectsMissingDeps(t *testing.T) {
	m := mock.New()
	_, _, js := testutil.StartTestJetStream(t)
	vpcVeth, _, err := handlers_imds.InitBuckets(js, 1)
	require.NoError(t, err)
	store := handlers_imds.NewVethStore(vpcVeth)

	_, err = NewIMDSTopologyManager(nil, policy.NewRouteManager(m), store)
	require.Error(t, err)
	_, err = NewIMDSTopologyManager(m, nil, store)
	require.Error(t, err)
	_, err = NewIMDSTopologyManager(m, policy.NewRouteManager(m), nil)
	require.Error(t, err)
}

func TestIMDSEnsureForVPC_InstallsTopology(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedVPCRouter(t, m, "vpc-0a1b2c3d4e5f6789", "10.0.0.0/16")
	mgr, store := newIMDSManager(t, m)

	spec, err := mgr.EnsureForVPC(ctx, "vpc-0a1b2c3d4e5f6789")
	require.NoError(t, err)

	// Resolved names are deterministic functions of the VPC ID.
	assert.Equal(t, "imds-ls-vpc-0a1b2c3d4e5f6789", spec.LSName)
	assert.Equal(t, "imds-lrp-vpc-0a1b2c3d4e5f6789", spec.LRPName)
	assert.Equal(t, "imds-port-vpc-0a1b2c3d4e5f6789", spec.LSPName)
	assert.Equal(t, "4e5f6789", spec.ShortVPCID)
	assert.Equal(t, "imds-ovs-4e5f6789", spec.OVSEndName)
	assert.Equal(t, "imds-h-4e5f6789", spec.HostEndName)

	// Logical switch exists, tagged as IMDS.
	ls, err := m.GetLogicalSwitch(ctx, spec.LSName)
	require.NoError(t, err)
	assert.Equal(t, "imds", ls.ExternalIDs["spinifex:role"])

	// LRP on the VPC router holds the /30.
	lrp, err := m.GetLogicalRouterPort(ctx, spec.LRPName)
	require.NoError(t, err)
	assert.Equal(t, []string{imdsLRPNetwork}, lrp.Networks)

	// type=router LSP peers the switch with the LRP.
	rlsp, err := m.GetLogicalSwitchPort(ctx, spec.RouterLSPName)
	require.NoError(t, err)
	assert.Equal(t, "router", rlsp.Type)
	assert.Equal(t, spec.LRPName, rlsp.Options["router-port"])

	// Host-owned localport: claims the MetaData IP, no port_security.
	lsp, err := m.GetLogicalSwitchPort(ctx, spec.LSPName)
	require.NoError(t, err)
	assert.Equal(t, "localport", lsp.Type)
	assert.Empty(t, lsp.PortSecurity)
	assert.Equal(t, []string{spec.LSPMAC + " " + handlers_imds.MetaDataServerIP}, lsp.Addresses)

	// Static route on vpc-{vpcID} points 169.254.169.254/32 at the IMDS LRP.
	route := findIMDSRoute(m, handlers_imds.MetaDataServerIP+"/32")
	require.NotNil(t, route)
	assert.Equal(t, handlers_imds.MetaDataServerIP, route.Nexthop)
	require.NotNil(t, route.OutputPort)
	assert.Equal(t, spec.LRPName, *route.OutputPort)

	// Record published to the bucket.
	rec, err := store.Get("vpc-0a1b2c3d4e5f6789")
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, spec.ShortVPCID, rec.ShortVPCID)
	assert.Equal(t, spec.LSPMAC, rec.IMDSPortMAC)
	assert.Equal(t, imdsLRPNetwork, rec.LRPNetwork)
	assert.NotEmpty(t, rec.CreatedAt)
}

func TestIMDSEnsureForVPC_Idempotent(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedVPCRouter(t, m, "vpc-1", "10.0.0.0/16")
	mgr, _ := newIMDSManager(t, m)

	for range 3 {
		_, err := mgr.EnsureForVPC(ctx, "vpc-1")
		require.NoError(t, err)
	}

	// Exactly one IMDS switch and one static route.
	assert.Len(t, m.Switches, 1)
	count := 0
	for _, r := range m.StaticRoutes {
		if r.IPPrefix == handlers_imds.MetaDataServerIP+"/32" {
			count++
		}
	}
	assert.Equal(t, 1, count)
	lsp, err := m.GetLogicalSwitchPort(ctx, "imds-port-vpc-1")
	require.NoError(t, err)
	assert.Equal(t, "localport", lsp.Type)
}

func TestIMDSEnsureForVPC_MissingRouterErrors(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	// No router seeded — the LRP create has nowhere to land.
	mgr, store := newIMDSManager(t, m)

	_, err := mgr.EnsureForVPC(ctx, "vpc-missing")
	require.Error(t, err)

	// No record published when install fails.
	rec, err := store.Get("vpc-missing")
	require.NoError(t, err)
	assert.Nil(t, rec)
}

func TestIMDSRemoveForVPC_TearsDownAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedVPCRouter(t, m, "vpc-1", "10.0.0.0/16")
	mgr, store := newIMDSManager(t, m)

	_, err := mgr.EnsureForVPC(ctx, "vpc-1")
	require.NoError(t, err)

	require.NoError(t, mgr.RemoveForVPC(ctx, "vpc-1"))

	assert.Empty(t, m.Switches)
	assert.Nil(t, findIMDSRoute(m, handlers_imds.MetaDataServerIP+"/32"))
	rec, err := store.Get("vpc-1")
	require.NoError(t, err)
	assert.Nil(t, rec)

	// Second remove is a no-op.
	require.NoError(t, mgr.RemoveForVPC(ctx, "vpc-1"))
}
