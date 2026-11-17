package ovn_test

import (
	"context"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn/nbdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func peeringEnds(pcxID, guardMatchA, guardMatchB string) [2]ovn.PeeringLinkEnd {
	peerA, peerB := "pcx-a", "pcx-b"
	ids := map[string]string{ovn.ExternalIDPcxID: pcxID}
	guardIDs := map[string]string{ovn.ExternalIDPcxID: pcxID, "spinifex:role": ovn.PeeringGuardRole}
	return [2]ovn.PeeringLinkEnd{
		{
			Router: "lr-a",
			Port:   nbdb.LogicalRouterPort{Name: peerA, MAC: "0a:00:00:00:02:01", Networks: []string{"169.254.192.1/30"}, Peer: &peerB, ExternalIDs: ids},
			Guard:  nbdb.LogicalRouterPolicy{Priority: 1400, Match: guardMatchA, Action: "drop", ExternalIDs: guardIDs},
		},
		{
			Router: "lr-b",
			Port:   nbdb.LogicalRouterPort{Name: peerB, MAC: "0a:00:00:00:02:02", Networks: []string{"169.254.192.2/30"}, Peer: &peerA, ExternalIDs: ids},
			Guard:  nbdb.LogicalRouterPolicy{Priority: 1400, Match: guardMatchB, Action: "drop", ExternalIDs: guardIDs},
		},
	}
}

func routerRows(t *testing.T, cli ovn.Client, ctx context.Context, router string) (ports int, policies []nbdb.LogicalRouterPolicy) {
	t.Helper()
	lr, err := cli.GetLogicalRouter(ctx, router)
	require.NoError(t, err)
	policies, err = cli.ListLogicalRouterPolicies(ctx, router)
	require.NoError(t, err)
	return len(lr.Ports), policies
}

func conformPeeringLink(t *testing.T, cli ovn.Client, ctx context.Context) {
	seedRouter(t, cli, ctx, "lr-a")
	seedRouter(t, cli, ctx, "lr-b")

	require.NoError(t, cli.EnsurePeeringLink(ctx, peeringEnds("pcx-1", `inport == "pcx-a" && ip4.src != 10.2.0.0/16`, `inport == "pcx-b" && ip4.src != 10.1.0.0/16`)))
	a, err := cli.GetLogicalRouterPort(ctx, "pcx-a")
	require.NoError(t, err)
	require.NotNil(t, a.Peer)
	assert.Equal(t, "pcx-b", *a.Peer)
	assert.Equal(t, []string{"169.254.192.1/30"}, a.Networks)
	ports, policies := routerRows(t, cli, ctx, "lr-b")
	assert.Equal(t, 1, ports)
	require.Len(t, policies, 1)
	assert.Equal(t, "drop", policies[0].Action)

	// A re-ensure rewrites in place and replaces a guard whose match changed.
	ends := peeringEnds("pcx-1", `inport == "pcx-a" && ip4.src != 10.2.0.0/16`, `inport == "pcx-b" && ip4.src != 10.9.0.0/16`)
	ends[1].Port.Networks = []string{"169.254.192.6/30"}
	require.NoError(t, cli.EnsurePeeringLink(ctx, ends))
	b, err := cli.GetLogicalRouterPort(ctx, "pcx-b")
	require.NoError(t, err)
	assert.Equal(t, []string{"169.254.192.6/30"}, b.Networks)
	ports, policies = routerRows(t, cli, ctx, "lr-b")
	assert.Equal(t, 1, ports)
	require.Len(t, policies, 1, "the superseded guard is removed")
	assert.Contains(t, policies[0].Match, "10.9.0.0/16")

	// A policy another owner placed on the router survives the delete.
	require.NoError(t, cli.AddLogicalRouterPolicy(ctx, "lr-a", &nbdb.LogicalRouterPolicy{Priority: 1100, Match: "ip4.dst == 0.0.0.0/0", Action: "drop"}))
	require.NoError(t, cli.AddLogicalRouterPolicy(ctx, "lr-a", &nbdb.LogicalRouterPolicy{
		Priority: 1333, Match: "ip4.dst == 10.2.0.0/16", Action: "reroute", Nexthop: strPtr("169.254.192.2"),
		ExternalIDs: map[string]string{ovn.ExternalIDPcxID: "pcx-1"},
	}))
	require.NoError(t, cli.DeletePeeringLink(ctx, "pcx-1", []string{"lr-a", "lr-b", "lr-gone"}))
	for _, router := range []string{"lr-a", "lr-b"} {
		ports, policies := routerRows(t, cli, ctx, router)
		assert.Zero(t, ports, router)
		for _, p := range policies {
			assert.NotEqual(t, "pcx-1", p.ExternalIDs[ovn.ExternalIDPcxID], router)
		}
	}
	_, policies = routerRows(t, cli, ctx, "lr-a")
	assert.Len(t, policies, 1)
	require.NoError(t, cli.DeletePeeringLink(ctx, "pcx-1", []string{"lr-a", "lr-b"}), "a repeated delete is a no-op")

	// A link to a router that does not exist writes nothing on the other.
	ends = peeringEnds("pcx-2", "a", "b")
	ends[1].Router = "lr-gone"
	require.Error(t, cli.EnsurePeeringLink(ctx, ends))
	ports, _ = routerRows(t, cli, ctx, "lr-a")
	assert.Zero(t, ports)
}

// An update rewrites a policy row's action, next hop and external IDs in
// place, keeping its identity; a missing row is an error.
func conformRouterPolicyUpdate(t *testing.T, cli ovn.Client, ctx context.Context) {
	seedRouter(t, cli, ctx, "lr-a")
	match := `inport == "rtr-subnet-1" && ip4.dst == 10.1.0.0/16`
	require.NoError(t, cli.AddLogicalRouterPolicy(ctx, "lr-a", &nbdb.LogicalRouterPolicy{
		Priority: 1332, Match: match, Action: "drop", ExternalIDs: map[string]string{"spinifex:rtb_id": "rtb-1"},
	}))
	before, err := cli.FindLogicalRouterPolicy(ctx, "lr-a", 1332, match)
	require.NoError(t, err)
	require.NotNil(t, before)

	nexthop := "169.254.192.2"
	require.NoError(t, cli.UpdateLogicalRouterPolicy(ctx, "lr-a", &nbdb.LogicalRouterPolicy{
		Priority: 1332, Match: match, Action: "reroute", Nexthop: &nexthop,
		ExternalIDs: map[string]string{"spinifex:rtb_id": "rtb-2"},
	}))
	got, err := cli.FindLogicalRouterPolicy(ctx, "lr-a", 1332, match)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, before.UUID, got.UUID)
	assert.Equal(t, "reroute", got.Action)
	require.NotNil(t, got.Nexthop)
	assert.Equal(t, nexthop, *got.Nexthop)
	assert.Equal(t, "rtb-2", got.ExternalIDs["spinifex:rtb_id"])

	err = cli.UpdateLogicalRouterPolicy(ctx, "lr-a", &nbdb.LogicalRouterPolicy{Priority: 1332, Match: "ip4", Action: "drop"})
	assert.Error(t, err)
}
