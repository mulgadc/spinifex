package dns

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	reconcilelock "github.com/mulgadc/spinifex/spinifex/domains/network/reconcile"
)

func TestDelegateWritesAndWithdrawsPlatformDelegations(t *testing.T) {
	w, _ := newMultiNodeTestWriter(t)
	_, err := w.Delegate(t.Context(), nil, "node1", []string{"a.r53e2e.spx3.net", "B.r53e2e.spx3.net."}, false)
	require.NoError(t, err)

	for _, name := range []string{"a.r53e2e.spx3.net.", "b.r53e2e.spx3.net"} {
		servers, zone, ok, err := w.PlatformDelegation(name)
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t, "spx3.net", zone)
		assert.ElementsMatch(t, []string{"ns1.spx3.net.", "ns2.spx3.net."}, servers, name)
	}
	servers, _, ok, err := w.PlatformDelegation("c.r53e2e.spx3.net")
	require.NoError(t, err)
	assert.True(t, ok, "a name below the base domain is decided by the base zone")
	assert.Empty(t, servers)

	_, err = w.Delegate(t.Context(), nil, "node1", []string{"a.r53e2e.spx3.net"}, true)
	require.NoError(t, err)
	servers, _, _, err = w.PlatformDelegation("a.r53e2e.spx3.net")
	require.NoError(t, err)
	assert.Empty(t, servers)
	servers, _, _, err = w.PlatformDelegation("b.r53e2e.spx3.net")
	require.NoError(t, err)
	assert.Len(t, servers, 2, "withdrawing one delegation leaves the others")
}

func TestPlatformDelegationIgnoresNamesOutsideTheBaseDomain(t *testing.T) {
	w, _ := newMultiNodeTestWriter(t)
	for _, name := range []string{"example.com", "spx3.net", "notspx3.net"} {
		_, _, ok, err := w.PlatformDelegation(name)
		require.NoError(t, err)
		assert.False(t, ok, name)
	}
}

func TestDelegateRefusesPlatformAndForeignNames(t *testing.T) {
	w, _ := newMultiNodeTestWriter(t)
	for _, name := range []string{"ns1.spx3.net", "x.elb.spx3.net", "compute.spx3.net", "example.com", "spx3.net"} {
		_, err := w.Delegate(t.Context(), nil, "node1", []string{"ok.spx3.net", name}, false)
		require.Error(t, err, name)
	}
	servers, _, _, err := w.PlatformDelegation("ok.spx3.net")
	require.NoError(t, err)
	assert.Empty(t, servers, "a refused batch writes nothing")
}

func TestDelegateWaitsForTheZoneWriterElection(t *testing.T) {
	w, _ := newMultiNodeTestWriter(t)
	_, nc, _ := testutil.StartTestJetStream(t)
	prev := delegateLockPoll
	delegateLockPoll = 10 * time.Millisecond
	t.Cleanup(func() { delegateLockPoll = prev })

	release, ok := reconcilelock.AcquireLeader(t.Context(), nc, KVBucketDNSReconcile, "node2")
	require.True(t, ok)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err := w.Delegate(ctx, nc, "node1", []string{"a.spx3.net"}, false)
	require.ErrorContains(t, err, "another node is writing the zone")

	release()
	_, err = w.Delegate(t.Context(), nc, "node1", []string{"a.spx3.net"}, false)
	require.NoError(t, err)
	servers, _, _, err := w.PlatformDelegation("a.spx3.net")
	require.NoError(t, err)
	assert.Len(t, servers, 2)
}
