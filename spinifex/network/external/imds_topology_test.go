package external

import (
	"context"
	"net/netip"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	handlers_imds "github.com/mulgadc/spinifex/spinifex/handlers/imds"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
	"github.com/mulgadc/spinifex/spinifex/testutil"
)

const (
	imdsTestSubnetID = "subnet-0a1b2c3d4e5f6789"
	imdsTestVPCID    = "vpc-0a1b2c3d4e5f6789"
	imdsTestCIDR     = "10.0.1.0/24"
)

func newIMDSManager(t *testing.T, m *mock.Client) (IMDSTopologyManager, *handlers_imds.VethStore) {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	subnetVeth, _, err := handlers_imds.InitBuckets(js, 1)
	require.NoError(t, err)
	store := handlers_imds.NewVethStore(subnetVeth)
	mgr, err := NewIMDSTopologyManager(m, store)
	require.NoError(t, err)
	return mgr, store
}

// seedSubnetSwitch creates the subnet logical switch the IMDS localport attaches
// to. The subnet lifecycle owns the switch; EnsureForSubnet only adds the localport.
func seedSubnetSwitch(t *testing.T, m *mock.Client, subnetID string) {
	t.Helper()
	_, err := m.EnsureLogicalSwitch(context.Background(), &nbdb.LogicalSwitch{
		Name:        topology.SubnetSwitch(subnetID),
		ExternalIDs: map[string]string{"spinifex:subnet_id": subnetID},
	})
	require.NoError(t, err)
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
	subnetVeth, _, err := handlers_imds.InitBuckets(js, 1)
	require.NoError(t, err)
	store := handlers_imds.NewVethStore(subnetVeth)

	_, err = NewIMDSTopologyManager(nil, store)
	require.Error(t, err)
	_, err = NewIMDSTopologyManager(m, nil)
	require.Error(t, err)
}

func TestIMDSEnsureForSubnet_InstallsLocalportOnSubnetLS(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedSubnetSwitch(t, m, imdsTestSubnetID)
	mgr, store := newIMDSManager(t, m)

	spec, err := mgr.EnsureForSubnet(ctx, imdsTestSubnetID, imdsTestVPCID, netip.MustParsePrefix(imdsTestCIDR))
	require.NoError(t, err)

	// Resolved names are deterministic functions of the subnet ID.
	assert.Equal(t, "subnet-"+imdsTestSubnetID, spec.LSName)
	assert.Equal(t, "imds-port-"+imdsTestSubnetID, spec.LSPName)
	assert.Equal(t, "4e5f6789", spec.ShortSubnetID)
	assert.Equal(t, "imds-o-4e5f6789", spec.OVSEndName)
	assert.Equal(t, "imds-h-4e5f6789", spec.HostEndName)

	// The localport is created directly on the guest's subnet switch — no IMDS
	// switch, LRP, router-LSP, or static route exists.
	assert.Len(t, m.Switches, 1, "only the subnet switch should exist; no dedicated IMDS switch")
	assert.Empty(t, m.RouterPorts, "no IMDS LRP under the L2 datapath")
	assert.Empty(t, m.StaticRoutes, "no 169.254.169.254/32 static route under the L2 datapath")
	assert.Nil(t, findIMDSRoute(m, handlers_imds.MetaDataServerIP+"/32"))

	// Host-owned localport: claims the MetaData IP, no port_security, no SG.
	lsp, err := m.GetLogicalSwitchPort(ctx, spec.LSPName)
	require.NoError(t, err)
	assert.Equal(t, "localport", lsp.Type)
	assert.Empty(t, lsp.PortSecurity)
	assert.Equal(t, []string{spec.LSPMAC + " " + handlers_imds.MetaDataServerIP}, lsp.Addresses)

	// ...and it is a member of the subnet switch.
	ls, err := m.GetLogicalSwitch(ctx, spec.LSName)
	require.NoError(t, err)
	assert.True(t, slices.Contains(ls.Ports, lsp.UUID), "localport must be a port on the subnet switch")

	// Record published to the bucket, keyed by subnet, carrying the owning VPC
	// (the subnet→VPC static lookup the IMDS handler needs) and the CIDR.
	rec, err := store.Get(imdsTestSubnetID)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, spec.ShortSubnetID, rec.ShortSubnetID)
	assert.Equal(t, imdsTestVPCID, rec.VPCID)
	assert.Equal(t, spec.LSPMAC, rec.IMDSPortMAC)
	assert.Equal(t, imdsTestCIDR, rec.SubnetCIDR)
	assert.NotEmpty(t, rec.CreatedAt)
}

func TestIMDSEnsureForSubnet_RequiresCIDR(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedSubnetSwitch(t, m, imdsTestSubnetID)
	mgr, _ := newIMDSManager(t, m)

	_, err := mgr.EnsureForSubnet(ctx, imdsTestSubnetID, imdsTestVPCID, netip.Prefix{})
	require.Error(t, err)
}

func TestIMDSEnsureForSubnet_RequiresVPCID(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedSubnetSwitch(t, m, imdsTestSubnetID)
	mgr, _ := newIMDSManager(t, m)

	// vpcID is persisted in the record for the IMDS handler's subnet→VPC lookup;
	// without it the eni-by-vpc-ip index can't be keyed, so install must fail loud.
	_, err := mgr.EnsureForSubnet(ctx, imdsTestSubnetID, "", netip.MustParsePrefix(imdsTestCIDR))
	require.Error(t, err)
}

func TestIMDSEnsureForSubnet_Idempotent(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedSubnetSwitch(t, m, "subnet-1")
	mgr, _ := newIMDSManager(t, m)

	for range 3 {
		_, err := mgr.EnsureForSubnet(ctx, "subnet-1", "vpc-1", netip.MustParsePrefix(imdsTestCIDR))
		require.NoError(t, err)
	}

	// Exactly one switch (the subnet) and exactly one localport.
	assert.Len(t, m.Switches, 1)
	assert.Len(t, m.Ports, 1)
	lsp, err := m.GetLogicalSwitchPort(ctx, "imds-port-subnet-1")
	require.NoError(t, err)
	assert.Equal(t, "localport", lsp.Type)
}

func TestIMDSEnsureForSubnet_MissingSwitchErrors(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	// No subnet switch seeded — the localport create has nowhere to land.
	mgr, store := newIMDSManager(t, m)

	_, err := mgr.EnsureForSubnet(ctx, "subnet-missing", "vpc-1", netip.MustParsePrefix(imdsTestCIDR))
	require.Error(t, err)

	// No record published when install fails.
	rec, err := store.Get("subnet-missing")
	require.NoError(t, err)
	assert.Nil(t, rec)
}

func TestIMDSRemoveForSubnet_TearsDownLocalportKeepsSwitch(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedSubnetSwitch(t, m, "subnet-1")
	mgr, store := newIMDSManager(t, m)

	spec, err := mgr.EnsureForSubnet(ctx, "subnet-1", "vpc-1", netip.MustParsePrefix(imdsTestCIDR))
	require.NoError(t, err)

	require.NoError(t, mgr.RemoveForSubnet(ctx, "subnet-1"))

	// Localport and record gone; the subnet switch survives (subnet lifecycle owns it).
	_, err = m.GetLogicalSwitchPort(ctx, spec.LSPName)
	require.Error(t, err)
	_, err = m.GetLogicalSwitch(ctx, spec.LSName)
	require.NoError(t, err)
	rec, err := store.Get("subnet-1")
	require.NoError(t, err)
	assert.Nil(t, rec)

	// Second remove is a no-op.
	require.NoError(t, mgr.RemoveForSubnet(ctx, "subnet-1"))
}
