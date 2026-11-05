//test:in-package — the election gate lives inside reconcileOnce, so driving it
//needs the unexported Reconciler fields and the writer's resolved S3 config.

package dns

import (
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	reconcilelock "github.com/mulgadc/spinifex/spinifex/domains/network/reconcile"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvsync"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// electingReconcilerFor is reconcilerFor with a lease, so a pass runs the
// election instead of skipping it.
func electingReconcilerFor(w *Writer, lease *kvsync.Lease, desired func() DesiredSet) *Reconciler {
	r := reconcilerFor(w, desired)
	r.lease = lease
	return r
}

// dnsLease returns holder's DNS lease on a store of its own, standing in for
// a separate process.
func dnsLease(js jetstream.JetStream, holder string) *kvsync.Lease {
	return NewReconcileLease(kvstore.OpenSyncBucket(js), holder)
}

// The zone lock is gone, and what replaces it is that a zone has one writer:
// the node holding the reconcile lease. A node that loses the election must
// therefore not touch the object at all — if it did, two nodes could be
// read-modify-writing the same zone with nothing serialising them.
func TestReconcileOnce_ALoserOfTheElectionDoesNotWriteTheZone(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	w, objects := newTestWriter(t)
	before := objects[testBase+".toml"]
	require.NotEmpty(t, before)

	// A peer holds the lease for the whole pass, so this node cannot win it.
	release, elected := reconcilelock.AcquireLeader(t.Context(), dnsLease(js, "peer"))
	require.True(t, elected, "the peer has to hold the lease or this test proves nothing")

	r := electingReconcilerFor(w, dnsLease(js, "node1"), func() DesiredSet {
		return DesiredSet{Changes: []Change{upsert("lb-1.elb."+testBase, "10.200.1.9")}}
	})
	r.reconcileOnce(t.Context())

	assert.Equal(t, before, objects[testBase+".toml"],
		"a node that did not win the election must leave the zone object exactly as it found it")

	release()
}

// The counterpart, so the test above is not passing because the pass was broken
// for some other reason: with the lease free, the same reconciler writes.
func TestReconcileOnce_TheWinnerOfTheElectionWritesTheZone(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	w, objects := newTestWriter(t)

	r := electingReconcilerFor(w, dnsLease(js, "node1"), func() DesiredSet {
		return DesiredSet{Changes: []Change{upsert("lb-1.elb."+testBase, "10.200.1.9")}}
	})
	r.reconcileOnce(t.Context())

	assert.Contains(t, objects[testBase+".toml"], `address = "10.200.1.9"`,
		"an uncontended pass has to reach the zone object")
}
