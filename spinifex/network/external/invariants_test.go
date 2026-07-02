package external

import (
	"context"
	"fmt"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

// TestI4_SubnetEgressRerouteMustExcludeLinkLocal asserts every per-subnet
// internet-egress reroute policy excludes 169.254.0.0/16. The reroute matches
// 0.0.0.0/0 — the widest possible scope — so without this exclusion it would
// SNAT-reroute link-local traffic out the WAN. Link-local is host-scoped by
// definition and must never egress to the provider network. Both the IGW- and
// NATGW-priority reroutes carry the gate, so both must spare link-local. A
// drifted exclude list silently leaks 169.254.0.0/16 onto the internet.
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
				"%s egress reroute %q does not exclude %s — it will SNAT-reroute "+
					"link-local traffic out the WAN; link-local must never egress",
				tc.name, policies[0].Match, linkLocalCIDR)
		})
	}
}
