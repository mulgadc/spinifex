package daemon

import (
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	admissionquota "github.com/mulgadc/spinifex/spinifex/domains/admission/quota"
	reconcilelock "github.com/mulgadc/spinifex/spinifex/domains/network/reconcile"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every lease owner's key is constructed against one store here, which the
// registry panics on if two of them collide. vpcd and the gateway run in
// processes of their own, so theirs are added by hand.
func TestDaemonLocks_EveryOwnerHasItsOwnKey(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	store := kvstore.OpenSyncBucket(testutil.NewJetStream(t, nc))
	assert.NotPanics(t, func() {
		locks := newDaemonLocks(store, "node-1")
		reconcilelock.NewLeaderLease(store, reconcilelock.KeyVPCDReconcile, "node-1")
		reconcilelock.NewLeaderLease(store, admissionquota.KeyReconcile, "node-1")
		locks.eks.Get("123456789012/alpha")
		locks.eks.Get("123456789012/alpha/teardown")
		locks.uefi.Get("i-0123456789abcdef0")
	})
}

// upgradedBuckets are the buckets a cluster upgraded from per-election leases
// carries, named here rather than read off obsoleteBuckets, so dropping a name
// from that list fails the test instead of leaving the bucket behind.
var upgradedBuckets = []string{
	"spinifex-dhcp-leases",
	"spinifex-vpcd-reconcile",
	"spinifex-dns-reconcile",
	"spinifex-route53-realize",
	"spinifex-route53-verify",
	"spinifex-quota-reconcile",
	"spinifex-rds-leader",
	"spinifex-ecs-leader",
	"bedrock-leader",
	"spinifex-eks-leader",
}

// A cluster upgraded from the per-election buckets carries each of them, and
// start removes every one while leaving other buckets alone.
func TestDeleteObsoleteBuckets_RemovesEveryOldLeaseBucket(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	for _, name := range append([]string{"spinifex-instance-state"}, upgradedBuckets...) {
		_, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: name})
		require.NoError(t, err, name)
	}

	deleteObsoleteBuckets(t.Context(), js)
	deleteObsoleteBuckets(t.Context(), js) // idempotent once they are gone

	for _, name := range upgradedBuckets {
		_, err := js.KeyValue(t.Context(), name)
		assert.ErrorIs(t, err, jetstream.ErrBucketNotFound, "%s must be deleted", name)
	}
	_, err := js.KeyValue(t.Context(), "spinifex-instance-state")
	assert.NoError(t, err, "a bucket not on the list must survive")
}
