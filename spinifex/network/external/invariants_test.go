package external

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	handlers_imds "github.com/mulgadc/spinifex/spinifex/handlers/imds"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

// IMDS-datapath invariants. These guard the OVN topology
// the IMDS handler trusts as a security + availability boundary. A regression
// in either is a privilege-escalation or single-point-of-failure vector, so the
// failure messages quote the offending design clause verbatim.

// TestI2_IMDSLocalportOnSubnetSwitchNoPortSecurity asserts the host-owned
// imds-port-{subnetID} LSP is a localport on the guest's subnet switch with no
// port_security. The host — not a guest — sources 169.254.169.254 frames on this
// port, so port_security would be actively harmful (it pins the allowed src MAC
// to the LSP's claimed MAC, and ovn-controller would drop reply frames egressing
// from the host veth's MAC). A regular (non-localport) LSP would bind to a single
// chassis, defeating the per-chassis self-serve design. Living on the subnet
// switch is the L2 datapath itself: the guest reaches metadata in one hop on its
// own broadcast domain, with no router in the path to shadow it.
func TestI2_IMDSLocalportOnSubnetSwitchNoPortSecurity(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedSubnetSwitch(t, m, imdsTestSubnetID)
	mgr, _ := newIMDSManager(t, m)

	spec, err := mgr.EnsureForSubnet(ctx, imdsTestSubnetID, imdsTestVPCID, netip.MustParsePrefix(imdsTestCIDR))
	require.NoError(t, err)

	lsp, err := m.GetLogicalSwitchPort(ctx, spec.LSPName)
	require.NoError(t, err)

	if lsp.Type != "localport" {
		t.Errorf("imds LSP %s Type = %q, want \"localport\": a regular LSP binds to "+
			"one chassis only, forcing IMDS through Geneve and creating a single point "+
			"of failure per subnet", spec.LSPName, lsp.Type)
	}
	if len(lsp.PortSecurity) != 0 {
		t.Errorf("imds LSP %s PortSecurity = %v, want empty: port_security set on the "+
			"host-owned LSP would cause ovn-controller to drop reply frames from the "+
			"host veth's MAC", spec.LSPName, lsp.PortSecurity)
	}
	assert.Equal(t, []string{spec.LSPMAC + " " + handlers_imds.MetaDataServerIP}, lsp.Addresses,
		"the localport must claim %s on the subnet switch", handlers_imds.MetaDataServerIP)

	// It must be a port on the subnet switch — that is what makes the datapath L2.
	ls, err := m.GetLogicalSwitch(ctx, spec.LSName)
	require.NoError(t, err)
	if !slices.Contains(ls.Ports, lsp.UUID) {
		t.Errorf("imds localport %s is not a port on subnet switch %s: the L2 datapath "+
			"requires the localport on the guest's own broadcast domain, not a dedicated "+
			"IMDS switch reached through the VPC router", spec.LSPName, spec.LSName)
	}
}

// TestI4_SubnetEgressRerouteMustExcludeLinkLocal asserts every per-subnet
// internet-egress reroute policy excludes 169.254.0.0/16. The reroute matches
// 0.0.0.0/0 — the widest possible scope — and fires in lr_in_policy AFTER the
// IMDS /32 static route matches in lr_in_ip_routing, so without this exclusion
// it overrides the /32 and SNAT-reroutes 169.254.169.254 out the WAN, where
// IMDS disappears (the guest's PUT for a token never reaches the per-VPC netns
// listener). Both the IGW- and NATGW-priority reroutes carry the gate, so both
// must spare link-local. A drifted exclude list silently breaks IMDS on any
// VPC with internet egress.
func TestI4_SubnetEgressRerouteMustExcludeLinkLocal(t *testing.T) {
	linkLocal := fmt.Sprintf("ip4.dst != %s", linkLocalCIDR.String())

	cases := []struct {
		name    string
		install func(mgr IGWManager, ctx context.Context) error
	}{
		{"IGW", func(mgr IGWManager, ctx context.Context) error {
			return mgr.EnsureSubnetEgress(ctx, "vpc-1", "subnet-pub", netip.MustParsePrefix("0.0.0.0/0"))
		}},
		{"NATGW", func(mgr IGWManager, ctx context.Context) error {
			return mgr.EnsureNATGatewaySubnetEgress(ctx, "vpc-1", "subnet-priv", netip.MustParsePrefix("0.0.0.0/0"))
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			m := mock.New()
			seedVPCRouter(t, m, "vpc-1", "10.0.0.0/16")
			pool := &ExternalPoolConfig{Name: "p", Gateway: "192.168.1.1", PrefixLen: 24}
			mgr, _ := newTestIGWManager(t, m, policy.NATModeDistributed, pool, LinkLocalAllocator{}, []string{"chassis-a"})

			require.NoError(t, mgr.AttachIGW(ctx, IGWSpec{VPCID: "vpc-1", InternetGatewayID: "igw-1"}))
			require.NoError(t, tc.install(mgr, ctx))

			policies, err := m.ListLogicalRouterPolicies(ctx, topology.VPCRouter("vpc-1"))
			require.NoError(t, err)
			require.Len(t, policies, 1)
			assert.Contains(t, policies[0].Match, linkLocal,
				"%s egress reroute %q does not exclude %s — it will hijack 169.254.169.254 "+
					"out the WAN and override the IMDS /32 static route, making IMDS unreachable",
				tc.name, policies[0].Match, linkLocalCIDR)
		})
	}
}
