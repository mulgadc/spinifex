//test:in-package — drives BindHostEIPs against nat_binder_test.go's in-package
// listingBinder and seedRouter fixtures, none of which the package exports.

package policy

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gatewayOn makes the binder answer "does this VPC's gateway run elsewhere"
// from a fixed verdict, standing in for the local ovn-controller binding.
func gatewayOn(b *listingBinder, elsewhere bool) HostEIPBinder {
	h := b.listHooks()
	h.GatewayElsewhere = func(string) (bool, error) { return elsewhere, nil }
	return h
}

func natGatewayAddress() []EIPSpec {
	return []EIPSpec{{VPCID: "vpc-1", ExternalIP: "192.168.1.200", NATGateway: true}}
}

// The gateway chassis is where the SNAT runs and where egress leaves, so it is
// the only node whose /32 route reaches anything.
func TestBindHostEIPsKeepsANATGatewayAddressOnItsGatewayChassis(t *testing.T) {
	b := &listingBinder{bound: []string{"192.168.1.200"}}
	mgr := routedManager(t, gatewayOn(b, false))

	require.NoError(t, mgr.BindHostEIPs(context.Background(), natGatewayAddress()))
	assert.Equal(t, []string{"192.168.1.200 via 100.127.0.10"}, b.binds,
		"the gateway chassis did not plumb its own NAT gateway")
	assert.Empty(t, b.unbinds)
}

// Anywhere else the route points into a datapath that never carries the
// address — and where it shares the nodes' own subnet, as on OCI, it outranks
// the connected route and swallows node-to-node traffic.
func TestBindHostEIPsDropsANATGatewayAddressWhoseChassisIsElsewhere(t *testing.T) {
	b := &listingBinder{bound: []string{"192.168.1.200"}}
	mgr := routedManager(t, gatewayOn(b, true))

	require.NoError(t, mgr.BindHostEIPs(context.Background(), natGatewayAddress()))
	assert.Empty(t, b.binds, "a node that is not the gateway chassis plumbed a NAT gateway")
	assert.Equal(t, []string{"192.168.1.200"}, b.unbinds,
		"the stale route was left behind on a node that lost the gateway")
}

// An unreadable Southbound is a lost signal, not a verdict: guessing
// "elsewhere" would take the gateway down on the node actually running it.
func TestBindHostEIPsKeepsANATGatewayAddressWhenTheChassisCannotBeRead(t *testing.T) {
	b := &listingBinder{bound: []string{"192.168.1.200"}}
	h := b.listHooks()
	h.GatewayElsewhere = func(string) (bool, error) {
		return false, fmt.Errorf("ovn-sbctl: connection refused")
	}
	mgr := routedManager(t, h)

	require.NoError(t, mgr.BindHostEIPs(context.Background(), natGatewayAddress()))
	assert.Empty(t, b.unbinds, "an unreadable chassis signal drove a teardown")
	assert.NotEmpty(t, b.binds, "an unreadable chassis signal blocked the bind")
}

// A binder with no gateway test is every deployment built before this existed,
// and a single-node cluster whose chassisredirect port is not yet claimed.
func TestBindHostEIPsIsUnchangedForABinderThatCannotAnswerTheGateway(t *testing.T) {
	b := &listingBinder{bound: []string{"192.168.1.200"}}
	mgr := routedManager(t, b.listHooks())

	require.NoError(t, mgr.BindHostEIPs(context.Background(), natGatewayAddress()))
	assert.NotEmpty(t, b.binds)
	assert.Empty(t, b.unbinds)
}

// The gateway test keys on the NAT gateway marker, not on the absent port: a
// centralised guest EIP is also portless and every node still plumbs it.
// Asking the gateway authority about a guest takes every address off every node
// but the one chassis that happens to hold the gateway.
func TestBindHostEIPsLeavesACentralisedGuestEIPOutOfTheGatewayTest(t *testing.T) {
	b := &listingBinder{bound: []string{"192.168.1.200"}}
	mgr := routedManager(t, gatewayOn(b, true))

	require.NoError(t, mgr.BindHostEIPs(context.Background(), []EIPSpec{
		{VPCID: "vpc-1", ExternalIP: "192.168.1.200", LogicalIP: "10.0.1.5"},
	}))
	assert.NotEmpty(t, b.binds, "a centralised guest EIP was skipped as a foreign gateway")
	assert.Empty(t, b.unbinds, "a centralised guest EIP was pruned as a foreign gateway")
}

// The same for a distributed guest EIP on the node running it, which is the
// shape every guest with a public address has.
func TestBindHostEIPsBindsADistributedGuestEIPOffTheGatewayChassis(t *testing.T) {
	b := &listingBinder{}
	h := ownedBy(b, "port-eni-1")
	h.GatewayElsewhere = func(string) (bool, error) { return true, nil }
	mgr := routedManager(t, h)

	require.NoError(t, mgr.BindHostEIPs(context.Background(), movedGuest()))
	assert.Equal(t, []string{"192.168.1.200 via "}, b.binds,
		"the node running the guest skipped its EIP because the VPC gateway is elsewhere")
}
