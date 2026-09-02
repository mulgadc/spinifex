package ovn_test

import (
	"context"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestClientConformance runs one behavioural suite against both ovn.Client
// implementations. LiveClient is the production path; mock.Client is what
// reconcile, policy, external, topology, subscribers and vpcd assert against,
// so any semantic drift between the two silently invalidates those suites.
//
// Only semantics both are contractually required to share live here. Cases
// where the mock deliberately models something libovsdb does differently (the
// referential-integrity rejections, Close leaving Connected true) stay in the
// per-implementation tests.
func TestClientConformance(t *testing.T) {
	impls := []struct {
		name string
		new  func(t *testing.T) (ovn.Client, context.Context)
	}{
		{"live", func(t *testing.T) (ovn.Client, context.Context) {
			cli, ctx := liveClient(t)
			return cli, ctx
		}},
		{"mock", func(t *testing.T) (ovn.Client, context.Context) {
			cli := mock.New()
			require.NoError(t, cli.Connect(t.Context()))
			return cli, t.Context()
		}},
	}

	suite := []struct {
		name string
		run  func(t *testing.T, cli ovn.Client, ctx context.Context)
	}{
		{"LogicalSwitch", conformLogicalSwitch},
		{"LogicalSwitchPort", conformLogicalSwitchPort},
		{"LogicalRouter", conformLogicalRouter},
		{"LogicalRouterPort", conformLogicalRouterPort},
		{"NAT", conformNAT},
		{"NATCrossRouter", conformNATCrossRouter},
		{"AddressSetAndNATExemption", conformAddressSetAndNATExemption},
		{"StaticRoutes", conformStaticRoutes},
		{"RouterPolicies", conformRouterPolicies},
		{"PortGroups", conformPortGroups},
		{"ACLs", conformACLs},
		{"GatewayChassis", conformGatewayChassis},
		{"DHCPOptions", conformDHCPOptions},
	}

	for _, impl := range impls {
		t.Run(impl.name, func(t *testing.T) {
			for _, tc := range suite {
				t.Run(tc.name, func(t *testing.T) {
					cli, ctx := impl.new(t)
					tc.run(t, cli, ctx)
				})
			}
		})
	}
}

// seedRouter creates a router and returns it, failing the test on error.
func seedRouter(t *testing.T, cli ovn.Client, ctx context.Context, name string) *nbdb.LogicalRouter {
	t.Helper()
	lr, created, err := cli.EnsureLogicalRouter(ctx, &nbdb.LogicalRouter{Name: name})
	require.NoError(t, err)
	require.True(t, created, "seed router %q already existed", name)
	return lr
}

// seedSwitch creates a switch and returns it, failing the test on error.
func seedSwitch(t *testing.T, cli ovn.Client, ctx context.Context, name string) *nbdb.LogicalSwitch {
	t.Helper()
	ls, created, err := cli.EnsureLogicalSwitch(ctx, &nbdb.LogicalSwitch{Name: name})
	require.NoError(t, err)
	require.True(t, created, "seed switch %q already existed", name)
	return ls
}

func conformLogicalSwitch(t *testing.T, cli ovn.Client, ctx context.Context) {
	ls := seedSwitch(t, cli, ctx, "ls-a")
	require.NotEmpty(t, ls.UUID, "Ensure must return a persisted UUID")

	// Re-ensure reuses the row rather than minting a second one with the same name.
	again, created, err := cli.EnsureLogicalSwitch(ctx, &nbdb.LogicalSwitch{Name: "ls-a"})
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, ls.UUID, again.UUID)

	got, err := cli.GetLogicalSwitch(ctx, "ls-a")
	require.NoError(t, err)
	assert.Equal(t, ls.UUID, got.UUID)

	seedSwitch(t, cli, ctx, "ls-b")
	all, err := cli.ListLogicalSwitches(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"ls-a", "ls-b"}, switchNames(all))

	require.NoError(t, cli.DeleteLogicalSwitch(ctx, "ls-a"))
	_, err = cli.GetLogicalSwitch(ctx, "ls-a")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"ls-a"`, "lookup error must name the missing switch")

	// Deleting an absent switch is an error on both, not a silent success.
	assert.Error(t, cli.DeleteLogicalSwitch(ctx, "ls-a"))
}

func conformLogicalSwitchPort(t *testing.T, cli ovn.Client, ctx context.Context) {
	seedSwitch(t, cli, ctx, "ls-p")

	require.NoError(t, cli.CreateLogicalSwitchPort(ctx, "ls-p", &nbdb.LogicalSwitchPort{
		Name:      "eni-1",
		Addresses: []string{"0a:00:00:00:00:01 10.0.0.5"},
	}))

	lsp, err := cli.GetLogicalSwitchPort(ctx, "eni-1")
	require.NoError(t, err)
	assert.Equal(t, []string{"0a:00:00:00:00:01 10.0.0.5"}, lsp.Addresses)

	// The port must be linked into the owning switch, not just created.
	ls, err := cli.GetLogicalSwitch(ctx, "ls-p")
	require.NoError(t, err)
	assert.Contains(t, ls.Ports, lsp.UUID)

	lsp.Addresses = []string{"0a:00:00:00:00:01 10.0.0.6"}
	require.NoError(t, cli.UpdateLogicalSwitchPort(ctx, lsp))
	updated, err := cli.GetLogicalSwitchPort(ctx, "eni-1")
	require.NoError(t, err)
	assert.Equal(t, []string{"0a:00:00:00:00:01 10.0.0.6"}, updated.Addresses)

	ports, err := cli.ListLogicalSwitchPorts(ctx)
	require.NoError(t, err)
	assert.Len(t, ports, 1)

	// Creating into a switch that does not exist must fail rather than leave an
	// orphan LSP behind.
	require.Error(t, cli.CreateLogicalSwitchPort(ctx, "no-such-switch", &nbdb.LogicalSwitchPort{Name: "eni-orphan"}))
	_, err = cli.GetLogicalSwitchPort(ctx, "eni-orphan")
	assert.Error(t, err, "failed create must not leave the port behind")

	require.NoError(t, cli.DeleteLogicalSwitchPort(ctx, "ls-p", "eni-1"))
	_, err = cli.GetLogicalSwitchPort(ctx, "eni-1")
	assert.Error(t, err)
	ls, err = cli.GetLogicalSwitch(ctx, "ls-p")
	require.NoError(t, err)
	assert.NotContains(t, ls.Ports, lsp.UUID, "delete must detach the port from the switch")
}

func conformLogicalRouter(t *testing.T, cli ovn.Client, ctx context.Context) {
	lr := seedRouter(t, cli, ctx, "lr-a")

	again, created, err := cli.EnsureLogicalRouter(ctx, &nbdb.LogicalRouter{Name: "lr-a"})
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, lr.UUID, again.UUID)

	require.NoError(t, cli.UpdateLogicalRouterExternalIDs(ctx, "lr-a", map[string]string{
		"spinifex:vpc_id": "vpc-1",
	}))
	got, err := cli.GetLogicalRouter(ctx, "lr-a")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"spinifex:vpc_id": "vpc-1"}, got.ExternalIDs)

	// The full merged set replaces what is there; it is not merged key-by-key.
	require.NoError(t, cli.UpdateLogicalRouterExternalIDs(ctx, "lr-a", map[string]string{
		"spinifex:az": "az-1",
	}))
	got, err = cli.GetLogicalRouter(ctx, "lr-a")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"spinifex:az": "az-1"}, got.ExternalIDs)

	assert.Error(t, cli.UpdateLogicalRouterExternalIDs(ctx, "no-such-router", nil))

	seedRouter(t, cli, ctx, "lr-b")
	all, err := cli.ListLogicalRouters(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"lr-a", "lr-b"}, routerNames(all))

	require.NoError(t, cli.DeleteLogicalRouter(ctx, "lr-a"))
	_, err = cli.GetLogicalRouter(ctx, "lr-a")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"lr-a"`)
	assert.Error(t, cli.DeleteLogicalRouter(ctx, "lr-a"))
}

func conformLogicalRouterPort(t *testing.T, cli ovn.Client, ctx context.Context) {
	seedRouter(t, cli, ctx, "lr-p")

	require.NoError(t, cli.CreateLogicalRouterPort(ctx, "lr-p", &nbdb.LogicalRouterPort{
		Name:     "lrp-1",
		MAC:      "0a:00:00:00:01:01",
		Networks: []string{"10.0.0.1/24"},
	}))

	lrp, err := cli.GetLogicalRouterPort(ctx, "lrp-1")
	require.NoError(t, err)
	assert.Equal(t, []string{"10.0.0.1/24"}, lrp.Networks)

	lr, err := cli.GetLogicalRouter(ctx, "lr-p")
	require.NoError(t, err)
	assert.Contains(t, lr.Ports, lrp.UUID)

	lrp.Networks = []string{"10.0.0.1/24", "10.0.1.1/24"}
	require.NoError(t, cli.UpdateLogicalRouterPort(ctx, lrp))
	updated, err := cli.GetLogicalRouterPort(ctx, "lrp-1")
	require.NoError(t, err)
	assert.Equal(t, []string{"10.0.0.1/24", "10.0.1.1/24"}, updated.Networks)

	all, err := cli.ListLogicalRouterPorts(ctx)
	require.NoError(t, err)
	assert.Len(t, all, 1)

	require.Error(t, cli.CreateLogicalRouterPort(ctx, "no-such-router", &nbdb.LogicalRouterPort{Name: "lrp-orphan"}))

	require.NoError(t, cli.DeleteLogicalRouterPort(ctx, "lr-p", "lrp-1"))
	_, err = cli.GetLogicalRouterPort(ctx, "lrp-1")
	assert.Error(t, err)
	lr, err = cli.GetLogicalRouter(ctx, "lr-p")
	require.NoError(t, err)
	assert.NotContains(t, lr.Ports, lrp.UUID)
}

func conformNAT(t *testing.T, cli ovn.Client, ctx context.Context) {
	seedRouter(t, cli, ctx, "lr-nat")
	seedRouter(t, cli, ctx, "lr-other")

	// Same (type, logical IP) on two routers: AWS CIDRs repeat across VPCs, so
	// every NAT operation keyed on logical IP must stay router-scoped.
	require.NoError(t, cli.AddNAT(ctx, "lr-nat", &nbdb.NAT{
		Type: "snat", ExternalIP: "100.127.0.10", LogicalIP: "10.0.0.0/16",
	}))
	require.NoError(t, cli.AddNAT(ctx, "lr-other", &nbdb.NAT{
		Type: "snat", ExternalIP: "100.127.0.20", LogicalIP: "10.0.0.0/16",
	}))

	nat, err := cli.FindNATByLogicalIP(ctx, "lr-nat", "snat", "10.0.0.0/16")
	require.NoError(t, err)
	require.NotNil(t, nat)
	assert.Equal(t, "100.127.0.10", nat.ExternalIP, "lookup must not return the other router's rule")

	missing, err := cli.FindNATByLogicalIP(ctx, "lr-nat", "snat", "10.9.0.0/16")
	require.NoError(t, err)
	assert.Nil(t, missing, "absent NAT is (nil, nil), not an error")

	byExt, err := cli.FindNATByExternalIP(ctx, "snat", "100.127.0.20")
	require.NoError(t, err)
	require.NotNil(t, byExt)
	assert.Equal(t, "10.0.0.0/16", byExt.LogicalIP)

	noExt, err := cli.FindNATByExternalIP(ctx, "snat", "100.127.0.99")
	require.NoError(t, err)
	assert.Nil(t, noExt)

	// A racing reconcile can mint a duplicate row; DeleteNAT must clear every
	// match on the router, not stop at the first.
	require.NoError(t, cli.AddNAT(ctx, "lr-nat", &nbdb.NAT{
		Type: "snat", ExternalIP: "100.127.0.10", LogicalIP: "10.0.0.0/16",
	}))
	all, err := cli.ListNATs(ctx)
	require.NoError(t, err)
	require.Len(t, all, 3, "expected the duplicate row to exist before delete")

	require.NoError(t, cli.DeleteNAT(ctx, "lr-nat", "snat", "10.0.0.0/16"))
	remaining, err := cli.ListNATs(ctx)
	require.NoError(t, err)
	require.Len(t, remaining, 1, "DeleteNAT must remove duplicates too")
	assert.Equal(t, "100.127.0.20", remaining[0].ExternalIP, "the other router's rule must survive")

	// The sentinel is what callers switch on for idempotent teardown.
	err = cli.DeleteNAT(ctx, "lr-nat", "snat", "10.0.0.0/16")
	assert.ErrorIs(t, err, ovn.ErrNATNotFound)

	err = cli.DeleteNATByExternalIP(ctx, "lr-nat", "snat", "100.127.0.20")
	assert.ErrorIs(t, err, ovn.ErrNATNotFound, "external-IP delete is router-scoped")

	require.NoError(t, cli.DeleteNATByExternalIP(ctx, "lr-other", "snat", "100.127.0.20"))
	remaining, err = cli.ListNATs(ctx)
	require.NoError(t, err)
	assert.Empty(t, remaining)
}

func conformNATCrossRouter(t *testing.T, cli ovn.Client, ctx context.Context) {
	seedRouter(t, cli, ctx, "lr-1")
	seedRouter(t, cli, ctx, "lr-2")

	// One EIP stranded across two VPC routers is exactly the stale-NAT case
	// DeleteAllNATsByExternalIP exists for.
	for _, r := range []string{"lr-1", "lr-2"} {
		require.NoError(t, cli.AddNAT(ctx, r, &nbdb.NAT{
			Type: "dnat_and_snat", ExternalIP: "100.127.0.30", LogicalIP: "10.0.0." + r[3:],
		}))
	}
	require.NoError(t, cli.AddNAT(ctx, "lr-1", &nbdb.NAT{
		Type: "dnat_and_snat", ExternalIP: "100.127.0.31", LogicalIP: "10.0.9.9",
	}))

	n, err := cli.DeleteAllNATsByExternalIP(ctx, "dnat_and_snat", "100.127.0.30")
	require.NoError(t, err)
	assert.Equal(t, 2, n, "must report how many rows it removed, across routers")

	remaining, err := cli.ListNATs(ctx)
	require.NoError(t, err)
	require.Len(t, remaining, 1)
	assert.Equal(t, "100.127.0.31", remaining[0].ExternalIP)

	// Nothing to delete is a zero count, not an error — callers treat it as
	// already-clean rather than a failure.
	n, err = cli.DeleteAllNATsByExternalIP(ctx, "dnat_and_snat", "100.127.0.30")
	require.NoError(t, err)
	assert.Zero(t, n)
}

func conformAddressSetAndNATExemption(t *testing.T, cli ovn.Client, ctx context.Context) {
	_, err := cli.GetAddressSet(ctx, "no-such-set")
	assert.ErrorIs(t, err, ovn.ErrAddressSetNotFound)

	uuid, err := cli.EnsureAddressSet(ctx, "spinifex_nat_exempt", []string{"100.127.0.0/24"})
	require.NoError(t, err)
	require.NotEmpty(t, uuid)

	// Re-ensuring converges the addresses in place; the UUID is a strong ref
	// held by NAT rows, so it must not change.
	uuid2, err := cli.EnsureAddressSet(ctx, "spinifex_nat_exempt", []string{"100.127.0.0/24", "192.168.1.0/24"})
	require.NoError(t, err)
	assert.Equal(t, uuid, uuid2)

	as, err := cli.GetAddressSet(ctx, "spinifex_nat_exempt")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"100.127.0.0/24", "192.168.1.0/24"}, as.Addresses)

	seedRouter(t, cli, ctx, "lr-ex")
	require.NoError(t, cli.AddNAT(ctx, "lr-ex", &nbdb.NAT{
		Type: "snat", ExternalIP: "100.127.0.10", LogicalIP: "10.0.0.0/16",
	}))

	require.NoError(t, cli.SetNATExemptedExtIPs(ctx, "lr-ex", "snat", "10.0.0.0/16", &uuid))
	nat, err := cli.FindNATByLogicalIP(ctx, "lr-ex", "snat", "10.0.0.0/16")
	require.NoError(t, err)
	require.NotNil(t, nat.ExemptedExtIps)
	assert.Equal(t, uuid, *nat.ExemptedExtIps)

	require.NoError(t, cli.SetNATExemptedExtIPs(ctx, "lr-ex", "snat", "10.0.0.0/16", nil))
	nat, err = cli.FindNATByLogicalIP(ctx, "lr-ex", "snat", "10.0.0.0/16")
	require.NoError(t, err)
	assert.Nil(t, nat.ExemptedExtIps, "nil must clear the ref")

	err = cli.SetNATExemptedExtIPs(ctx, "lr-ex", "snat", "10.9.0.0/16", &uuid)
	assert.ErrorIs(t, err, ovn.ErrNATNotFound)
}

func conformStaticRoutes(t *testing.T, cli ovn.Client, ctx context.Context) {
	seedRouter(t, cli, ctx, "lr-r1")
	seedRouter(t, cli, ctx, "lr-r2")

	// Every VPC router carries a default route, so prefix matching that is not
	// router-scoped grabs another VPC's row.
	require.NoError(t, cli.AddStaticRoute(ctx, "lr-r1", &nbdb.LogicalRouterStaticRoute{
		IPPrefix: "0.0.0.0/0", Nexthop: "100.127.0.1",
	}))
	require.NoError(t, cli.AddStaticRoute(ctx, "lr-r2", &nbdb.LogicalRouterStaticRoute{
		IPPrefix: "0.0.0.0/0", Nexthop: "100.127.0.2",
	}))

	r1, err := cli.FindStaticRoute(ctx, "lr-r1", "0.0.0.0/0")
	require.NoError(t, err)
	require.NotNil(t, r1)
	assert.Equal(t, "100.127.0.1", r1.Nexthop)

	absent, err := cli.FindStaticRoute(ctx, "lr-r1", "10.1.0.0/16")
	require.NoError(t, err)
	assert.Nil(t, absent, "absent route is (nil, nil), not an error")

	require.NoError(t, cli.DeleteStaticRoute(ctx, "lr-r1", "0.0.0.0/0"))
	gone, err := cli.FindStaticRoute(ctx, "lr-r1", "0.0.0.0/0")
	require.NoError(t, err)
	assert.Nil(t, gone)

	survivor, err := cli.FindStaticRoute(ctx, "lr-r2", "0.0.0.0/0")
	require.NoError(t, err)
	require.NotNil(t, survivor, "delete must not reach across routers")
	assert.Equal(t, "100.127.0.2", survivor.Nexthop)

	err = cli.DeleteStaticRoute(ctx, "lr-r1", "0.0.0.0/0")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "0.0.0.0/0")
}

func conformRouterPolicies(t *testing.T, cli ovn.Client, ctx context.Context) {
	seedRouter(t, cli, ctx, "lr-pol")

	empty, err := cli.ListLogicalRouterPolicies(ctx, "lr-pol")
	require.NoError(t, err)
	assert.Empty(t, empty)

	require.NoError(t, cli.AddLogicalRouterPolicy(ctx, "lr-pol", &nbdb.LogicalRouterPolicy{
		Priority: 100, Match: "ip4.src == 10.0.1.0/24", Action: "reroute",
		Nexthop: strPtr("10.0.0.9"),
	}))

	got, err := cli.FindLogicalRouterPolicy(ctx, "lr-pol", 100, "ip4.src == 10.0.1.0/24")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "reroute", got.Action)
	require.NotNil(t, got.Nexthop)
	assert.Equal(t, "10.0.0.9", *got.Nexthop)

	// Identity is the (router, priority, match) triple: a different priority or
	// match is a different policy, not the same one.
	noMatch, err := cli.FindLogicalRouterPolicy(ctx, "lr-pol", 200, "ip4.src == 10.0.1.0/24")
	require.NoError(t, err)
	assert.Nil(t, noMatch)

	all, err := cli.ListLogicalRouterPolicies(ctx, "lr-pol")
	require.NoError(t, err)
	assert.Len(t, all, 1)

	// Deleting an absent policy is a no-op, mirroring DeleteStaticRoute's
	// idempotent teardown contract.
	require.NoError(t, cli.DeleteLogicalRouterPolicy(ctx, "lr-pol", 999, "nothing"))

	require.NoError(t, cli.DeleteLogicalRouterPolicy(ctx, "lr-pol", 100, "ip4.src == 10.0.1.0/24"))
	gone, err := cli.FindLogicalRouterPolicy(ctx, "lr-pol", 100, "ip4.src == 10.0.1.0/24")
	require.NoError(t, err)
	assert.Nil(t, gone)

	_, err = cli.ListLogicalRouterPolicies(ctx, "no-such-router")
	assert.Error(t, err)
}

func conformPortGroups(t *testing.T, cli ovn.Client, ctx context.Context) {
	_, err := cli.GetPortGroup(ctx, "pg-missing")
	assert.ErrorIs(t, err, ovn.ErrPortGroupNotFound)

	pg, created, err := cli.EnsurePortGroup(ctx, "pg-sg-1", nil)
	require.NoError(t, err)
	require.True(t, created)
	require.NotEmpty(t, pg.UUID)

	again, created, err := cli.EnsurePortGroup(ctx, "pg-sg-1", nil)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, pg.UUID, again.UUID)

	require.NoError(t, cli.CreatePortGroup(ctx, "pg-sg-2", nil))
	groups, err := cli.ListPortGroups(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"pg-sg-1", "pg-sg-2"}, portGroupNames(groups))

	seedSwitch(t, cli, ctx, "ls-pg")

	// Joining at create time closes the window where the LSP exists outside
	// every group, which OVN treats as unrestricted.
	require.NoError(t, cli.CreateLogicalSwitchPortInGroups(ctx, "ls-pg",
		&nbdb.LogicalSwitchPort{Name: "eni-pg"}, []string{"pg-sg-1"}))

	names, err := cli.ListPortGroupsForPort(ctx, "eni-pg")
	require.NoError(t, err)
	assert.Equal(t, []string{"pg-sg-1"}, names)

	require.NoError(t, cli.UpdatePortGroupMemberships(ctx, "eni-pg", []string{"pg-sg-2"}, []string{"pg-sg-1"}))
	names, err = cli.ListPortGroupsForPort(ctx, "eni-pg")
	require.NoError(t, err)
	assert.Equal(t, []string{"pg-sg-2"}, names, "add and remove must both apply")

	require.NoError(t, cli.UpdatePortGroupMemberships(ctx, "eni-pg", nil, []string{"pg-sg-2"}))
	names, err = cli.ListPortGroupsForPort(ctx, "eni-pg")
	require.NoError(t, err)
	assert.Empty(t, names, "no memberships is an empty list, not an error")

	// A missing port group must abort the whole call rather than half-apply.
	assert.Error(t, cli.UpdatePortGroupMemberships(ctx, "eni-pg", []string{"pg-sg-1", "pg-nope"}, nil))

	_, err = cli.ListPortGroupsForPort(ctx, "eni-missing")
	assert.Error(t, err)

	require.NoError(t, cli.DeletePortGroup(ctx, "pg-sg-2"))
	_, err = cli.GetPortGroup(ctx, "pg-sg-2")
	assert.ErrorIs(t, err, ovn.ErrPortGroupNotFound)
}

func conformACLs(t *testing.T, cli ovn.Client, ctx context.Context) {
	_, _, err := cli.EnsurePortGroup(ctx, "pg-acl", nil)
	require.NoError(t, err)

	specs := []ovn.ACLSpec{
		{Direction: "to-lport", Priority: 1001, Match: "ip4.src == 10.0.0.0/16", Action: "allow-related", Name: "sg-ingress"},
		{Direction: "from-lport", Priority: 1002, Match: "ip4", Action: "allow-related"},
	}
	require.NoError(t, cli.AddACLs(ctx, "pg-acl", specs))
	assert.Len(t, aclUUIDs(t, ctx, cli, "pg-acl"), 2)

	// An unchanged security group must not churn ACL rows: the UUIDs staying
	// put is what stops ovn-northd recomputing flows on every drift tick.
	require.NoError(t, cli.ReplaceACLs(ctx, "pg-acl", specs))
	assert.Len(t, aclUUIDs(t, ctx, cli, "pg-acl"), 2)

	require.NoError(t, cli.ReplaceACLs(ctx, "pg-acl", specs[:1]))
	assert.Len(t, aclUUIDs(t, ctx, cli, "pg-acl"), 1)

	require.NoError(t, cli.ClearACLs(ctx, "pg-acl"))
	assert.Empty(t, aclUUIDs(t, ctx, cli, "pg-acl"))

	// Clearing an already-empty group is a no-op, so teardown can run twice.
	require.NoError(t, cli.ClearACLs(ctx, "pg-acl"))

	assert.Error(t, cli.AddACLs(ctx, "pg-nope", specs))
	assert.Error(t, cli.ReplaceACLs(ctx, "pg-nope", specs))
	assert.Error(t, cli.ClearACLs(ctx, "pg-nope"))
}

func conformGatewayChassis(t *testing.T, cli ovn.Client, ctx context.Context) {
	seedRouter(t, cli, ctx, "lr-gw")
	require.NoError(t, cli.CreateLogicalRouterPort(ctx, "lr-gw", &nbdb.LogicalRouterPort{
		Name: "lrp-gw", MAC: "0a:00:00:00:02:01", Networks: []string{"100.127.0.1/24"},
	}))

	absent, err := cli.GetGatewayChassisByName(ctx, "lrp-gw-node1")
	require.NoError(t, err)
	assert.Nil(t, absent, "absent chassis binding is (nil, nil)")

	// Three-way idempotence: create, then no-op on the same priority, then
	// update in place on a different one — never a second row for the pair.
	require.NoError(t, cli.SetGatewayChassis(ctx, "lrp-gw", "node1", 100))
	gc, err := cli.GetGatewayChassisByName(ctx, "lrp-gw-node1")
	require.NoError(t, err)
	require.NotNil(t, gc)
	assert.Equal(t, 100, gc.Priority)
	assert.Equal(t, "node1", gc.ChassisName)

	require.NoError(t, cli.SetGatewayChassis(ctx, "lrp-gw", "node1", 100))
	rows, err := cli.ListGatewayChassis(ctx)
	require.NoError(t, err)
	assert.Len(t, rows, 1)

	require.NoError(t, cli.SetGatewayChassis(ctx, "lrp-gw", "node1", 50))
	gc, err = cli.GetGatewayChassisByName(ctx, "lrp-gw-node1")
	require.NoError(t, err)
	require.NotNil(t, gc)
	assert.Equal(t, 50, gc.Priority)
	rows, err = cli.ListGatewayChassis(ctx)
	require.NoError(t, err)
	assert.Len(t, rows, 1, "priority change must update in place, not add a row")

	lrp, err := cli.GetLogicalRouterPort(ctx, "lrp-gw")
	require.NoError(t, err)
	assert.Contains(t, lrp.GatewayChassis, gc.UUID)

	assert.Error(t, cli.SetGatewayChassis(ctx, "no-such-lrp", "node1", 100))

	require.NoError(t, cli.DeleteGatewayChassis(ctx, "lrp-gw", gc.UUID))
	gone, err := cli.GetGatewayChassisByName(ctx, "lrp-gw-node1")
	require.NoError(t, err)
	assert.Nil(t, gone)
	lrp, err = cli.GetLogicalRouterPort(ctx, "lrp-gw")
	require.NoError(t, err)
	assert.NotContains(t, lrp.GatewayChassis, gc.UUID, "delete must detach from the LRP")
}

func conformDHCPOptions(t *testing.T, cli ovn.Client, ctx context.Context) {
	uuid, err := cli.CreateDHCPOptions(ctx, &nbdb.DHCPOptions{
		CIDR:        "10.0.1.0/24",
		Options:     map[string]string{"server_id": "10.0.1.1", "lease_time": "3600"},
		ExternalIDs: map[string]string{"spinifex:subnet_id": "subnet-1"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, uuid, "create must return the row UUID callers store")

	byCIDR, err := cli.FindDHCPOptionsByCIDR(ctx, "10.0.1.0/24")
	require.NoError(t, err)
	assert.Equal(t, uuid, byCIDR.UUID)
	assert.Equal(t, "10.0.1.1", byCIDR.Options["server_id"])

	byExtID, err := cli.FindDHCPOptionsByExternalID(ctx, "spinifex:subnet_id", "subnet-1")
	require.NoError(t, err)
	assert.Equal(t, uuid, byExtID.UUID)

	require.NoError(t, cli.UpdateDHCPOptionsOptions(ctx, uuid, map[string]string{
		"server_id": "10.0.1.1", "lease_time": "7200",
	}))
	byCIDR, err = cli.FindDHCPOptionsByCIDR(ctx, "10.0.1.0/24")
	require.NoError(t, err)
	assert.Equal(t, "7200", byCIDR.Options["lease_time"], "update must replace the options map")

	all, err := cli.ListDHCPOptions(ctx)
	require.NoError(t, err)
	assert.Len(t, all, 1)

	_, err = cli.FindDHCPOptionsByCIDR(ctx, "10.9.9.0/24")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "10.9.9.0/24")
	_, err = cli.FindDHCPOptionsByExternalID(ctx, "spinifex:subnet_id", "subnet-nope")
	assert.Error(t, err)

	require.NoError(t, cli.DeleteDHCPOptions(ctx, uuid))
	all, err = cli.ListDHCPOptions(ctx)
	require.NoError(t, err)
	assert.Empty(t, all)
}

func strPtr(s string) *string { return &s }

func switchNames(rows []nbdb.LogicalSwitch) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Name)
	}
	return out
}

func routerNames(rows []nbdb.LogicalRouter) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Name)
	}
	return out
}

func portGroupNames(rows []nbdb.PortGroup) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Name)
	}
	return out
}
