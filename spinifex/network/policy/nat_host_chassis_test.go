//test:in-package — drives BindHostEIPs against nat_binder_test.go's in-package
// listingBinder and seedRouter fixtures, none of which the package exports.

package policy

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/network/ovn/mock"
)

// ownedBy makes the binder answer "is this port on this chassis" from a fixed
// set, standing in for local OVS.
func ownedBy(b *listingBinder, local ...string) HostEIPBinder {
	h := b.listHooks()
	here := make(map[string]struct{}, len(local))
	for _, p := range local {
		here[p] = struct{}{}
	}
	h.Owns = func(portName string) (bool, error) {
		_, ok := here[portName]
		return ok, nil
	}
	return h
}

func routedManager(t *testing.T, hooks HostEIPBinder) *natManager {
	t.Helper()
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	seedGatewayPortIP(t, m, "vpc-1", "100.127.0.10", nil)
	mgr, err := NewNATManager(m, NATModeRouted, WithHostEIPBinder(hooks))
	require.NoError(t, err)
	return mgr.(*natManager)
}

func movedGuest() []EIPSpec {
	return []EIPSpec{{
		VPCID: "vpc-1", ExternalIP: "192.168.1.200", LogicalIP: "10.0.1.5",
		PortName: "port-eni-1", MAC: "52:54:00:aa:bb:cc",
	}}
}

// The address is still live cluster-wide, so the intent test alone keeps it and
// this node went on answering ARP for a guest that had moved away.
func TestBindHostEIPsDropsTheRouteWhenTheGuestHasMovedOff(t *testing.T) {
	b := &listingBinder{bound: []string{"192.168.1.200"}}
	mgr := routedManager(t, ownedBy(b)) // no local ports: the guest is elsewhere

	require.NoError(t, mgr.BindHostEIPs(context.Background(), movedGuest()))
	assert.Equal(t, []string{"192.168.1.200"}, b.unbinds,
		"a node kept the host plumbing for a guest running elsewhere")
}

// The node actually running the guest must keep it, or the sweep takes down the
// address it is responsible for.
func TestBindHostEIPsKeepsTheRouteOnTheNodeRunningTheGuest(t *testing.T) {
	b := &listingBinder{bound: []string{"192.168.1.200"}}
	mgr := routedManager(t, ownedBy(b, "port-eni-1"))

	require.NoError(t, mgr.BindHostEIPs(context.Background(), movedGuest()))
	assert.Empty(t, b.unbinds, "the owning node tore down its own guest's address")
}

// An unreadable local OVS is a lost signal, not a verdict. Guessing "foreign"
// would take a live address down on every node at once.
func TestBindHostEIPsKeepsTheRouteWhenOwnershipCannotBeRead(t *testing.T) {
	b := &listingBinder{bound: []string{"192.168.1.200"}}
	h := b.listHooks()
	h.Owns = func(string) (bool, error) { return false, fmt.Errorf("ovs-vsctl: connection refused") }
	mgr := routedManager(t, h)

	require.NoError(t, mgr.BindHostEIPs(context.Background(), movedGuest()))
	assert.Empty(t, b.unbinds, "an unreadable liveness signal drove a teardown")
}

// A binder with no chassis test is every deployment built before this existed,
// and it must behave exactly as it did.
func TestBindHostEIPsIsUnchangedForABinderThatCannotAnswerOwnership(t *testing.T) {
	b := &listingBinder{bound: []string{"192.168.1.200"}}
	mgr := routedManager(t, b.listHooks())

	require.NoError(t, mgr.BindHostEIPs(context.Background(), movedGuest()))
	assert.Empty(t, b.unbinds)
}

// A centralised EIP hairpins through the gateway chassis, so every node plumbs
// it legitimately and the chassis test must not fire.
func TestBindHostEIPsKeepsACentralisedAddressOnEveryNode(t *testing.T) {
	b := &listingBinder{bound: []string{"192.168.1.200"}}
	mgr := routedManager(t, ownedBy(b))

	specs := movedGuest()
	specs[0].MAC = "" // no external MAC: the row is not distributed
	require.NoError(t, mgr.BindHostEIPs(context.Background(), specs))
	assert.Empty(t, b.unbinds, "a centralised address was pruned as foreign")
}
