package handlers_bedrock

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSystemSubnet = "subnet-bedrocksys-0001"

func TestEnsureDaemonPort_CreatesENIAndHostPort(t *testing.T) {
	h := newLaunchHarness()
	require.NoError(t, EnsureDaemonPort(t.Context(), h.deps(), testSystemSubnet, "10.244.1.0/24"))

	require.Len(t, h.vpc.created, 1, "one daemon ENI should be minted")
	calls := h.hostPort.calls()
	require.Len(t, calls, 1)
	assert.Equal(t, h.vpc.created[0], calls[0].eniID)
	assert.Equal(t, "02:00:00:00:00:01", calls[0].mac)
}

// The port must carry the ENI address at the SUBNET's prefix length: a /32
// addresses the port and still leaves every serving VM unreachable, which is
// the entire reason the port exists.
func TestEnsureDaemonPort_UsesSubnetPrefixLength(t *testing.T) {
	h := newLaunchHarness()
	require.NoError(t, EnsureDaemonPort(t.Context(), h.deps(), testSystemSubnet, "10.244.1.0/24"))

	calls := h.hostPort.calls()
	require.Len(t, calls, 1)
	assert.True(t, strings.HasSuffix(calls[0].addr, "/24"),
		"host port address %q should carry the subnet's /24, not a host route", calls[0].addr)
	assert.False(t, strings.HasPrefix(calls[0].addr, "10.244.1.0/"),
		"host port address %q must be the ENI's own address, not the network address", calls[0].addr)
}

// One ENI per node, not one per launch: without describe-or-create every
// endpoint would leak an address out of the system subnet.
func TestEnsureDaemonPort_ReusesTheNodesExistingENI(t *testing.T) {
	h := newLaunchHarness()
	deps := h.deps()
	require.NoError(t, EnsureDaemonPort(t.Context(), deps, testSystemSubnet, "10.244.1.0/24"))
	require.NoError(t, EnsureDaemonPort(t.Context(), deps, testSystemSubnet, "10.244.1.0/24"))

	assert.Len(t, h.vpc.created, 1, "the second ensure should reuse the first ENI")
	calls := h.hostPort.calls()
	require.Len(t, calls, 2, "the host port is still re-driven, since a reboot clears it")
	assert.Equal(t, calls[0], calls[1], "both ensures should install the same port")
}

// Two daemons sharing one address would fight over the same OVN logical port,
// so the node identity has to reach the ENI lookup.
func TestEnsureDaemonPort_DistinctENIPerNode(t *testing.T) {
	h := newLaunchHarness()
	a := h.deps()
	a.NodeID = "node-a"
	b := h.deps()
	b.NodeID = "node-b"

	require.NoError(t, EnsureDaemonPort(t.Context(), a, testSystemSubnet, "10.244.1.0/24"))
	require.NoError(t, EnsureDaemonPort(t.Context(), b, testSystemSubnet, "10.244.1.0/24"))

	assert.Len(t, h.vpc.created, 2, "each node should own its own ENI")
	calls := h.hostPort.calls()
	require.Len(t, calls, 2)
	assert.NotEqual(t, calls[0].eniID, calls[1].eniID)
}

func TestEnsureDaemonPort_RequiresPlumberAndNodeID(t *testing.T) {
	h := newLaunchHarness()

	noPlumber := h.deps()
	noPlumber.HostPort = nil
	err := EnsureDaemonPort(t.Context(), noPlumber, testSystemSubnet, "10.244.1.0/24")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "host-port plumber")

	noNode := h.deps()
	noNode.NodeID = ""
	err = EnsureDaemonPort(t.Context(), noNode, testSystemSubnet, "10.244.1.0/24")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "node id")

	assert.Empty(t, h.vpc.created, "neither refusal should have minted an ENI")
}

func TestEnsureDaemonPort_RejectsUnparseableSubnetCIDR(t *testing.T) {
	h := newLaunchHarness()
	err := EnsureDaemonPort(t.Context(), h.deps(), testSystemSubnet, "not-a-cidr")
	require.Error(t, err)
	assert.Empty(t, h.vpc.created, "the CIDR is validated before anything is created")
}

func TestEnsureDaemonPort_PropagatesPlumberFailure(t *testing.T) {
	h := newLaunchHarness()
	h.hostPort.failErr = errors.New("ovs-vsctl exploded")
	err := EnsureDaemonPort(t.Context(), h.deps(), testSystemSubnet, "10.244.1.0/24")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ovs-vsctl exploded")
}

// An ENI record with no address or MAC cannot back a host port, so the lookup
// must fall through and mint a usable one rather than install a broken port.
func TestEnsureDaemonPort_SkipsIncompleteENIRecords(t *testing.T) {
	h := newLaunchHarness()
	h.vpc.putRecord("eni-broken", testSystemSubnet, daemonPortDescription(testNodeID), "", "")

	require.NoError(t, EnsureDaemonPort(t.Context(), h.deps(), testSystemSubnet, "10.244.1.0/24"))
	calls := h.hostPort.calls()
	require.Len(t, calls, 1)
	assert.NotEqual(t, "eni-broken", calls[0].eniID)
	assert.NotEmpty(t, calls[0].mac)
}

// A serving VM the daemon cannot dial is worse than no VM: it holds a GPU for
// the whole readiness window before unwinding. So the port comes first, and a
// port that cannot be installed stops the launch before anything is created.
func TestLaunchServingVM_RefusesWhenTheDaemonPortFails(t *testing.T) {
	h := newLaunchHarness()
	h.hostPort.failErr = errors.New("br-int is not there")

	_, err := LaunchServingVM(t.Context(), h.deps(), testLaunchInput())
	require.Error(t, err)
	assert.Empty(t, h.launcher.launches(), "no VM may be launched with no path to reach it")
	assert.Empty(t, h.volumes.created, "no weights volume may be cloned either")
}

// The daemon's own ENI must not be confused with a serving VM's: only the
// serving ENI is ever handed to the launcher.
func TestLaunchServingVM_DaemonPortENIIsNotTheServingENI(t *testing.T) {
	h := newLaunchHarness()
	out, err := LaunchServingVM(t.Context(), h.deps(), testLaunchInput())
	require.NoError(t, err)

	calls := h.hostPort.calls()
	require.Len(t, calls, 1)
	assert.NotEqual(t, out.ENIID, calls[0].eniID)
	assert.Contains(t, out.BaseURL, out.PrivateIP, "BaseURL still names the serving VM's own address")
}

// The description is what idempotence keys off, since DescribeNetworkInterfaces
// has no tag filter. A change to its shape silently orphans every existing port.
func TestDaemonPortDescriptionCarriesNodeID(t *testing.T) {
	assert.Contains(t, daemonPortDescription("node-a"), "node-a")
	assert.NotEqual(t, daemonPortDescription("node-a"), daemonPortDescription("node-b"))
}
