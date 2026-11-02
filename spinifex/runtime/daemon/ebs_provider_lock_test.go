package daemon

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newLockManager opens the cluster-config bucket over its own connection, as
// a separate daemon would.
func newLockManager(t *testing.T, url string) *JetStreamManager {
	t.Helper()
	nc, err := nats.Connect(url)
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	m, err := NewJetStreamManager(nc)
	require.NoError(t, err)
	require.NoError(t, m.InitClusterConfigBucket())
	return m
}

func recordedEBSProvider(t *testing.T, js jetstream.JetStream) (EBSProviderRecord, uint64) {
	t.Helper()
	kv, err := js.KeyValue(t.Context(), ClusterConfigBucket)
	require.NoError(t, err)
	entry, err := kv.Get(t.Context(), EBSProviderKey)
	require.NoError(t, err)
	var rec EBSProviderRecord
	require.NoError(t, json.Unmarshal(entry.Value(), &rec))
	return rec, entry.Revision()
}

func TestClusterConfigBucket_NoTTLAndSchemaVersion(t *testing.T) {
	ns, _, js := testutil.StartTestJetStream(t)
	newLockManager(t, ns.ClientURL())

	kv, err := js.KeyValue(t.Context(), ClusterConfigBucket)
	require.NoError(t, err)
	status, err := kv.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(1), status.History())
	assert.Zero(t, status.TTL(), "a TTL would expire the lock and let a second provider in")

	v, err := kvutil.ReadVersion(t.Context(), kv)
	require.NoError(t, err)
	assert.Equal(t, 1, v)
}

func TestClaimEBSProvider_FirstClaimRecordsAndMatchingNodesJoin(t *testing.T) {
	ns, _, js := testutil.StartTestJetStream(t)
	first := newLockManager(t, ns.ClientURL())
	second := newLockManager(t, ns.ClientURL())

	require.NoError(t, first.ClaimEBSProvider(t.Context(), "node1", config.EBSProviderQEMUNBD))
	_, rev := recordedEBSProvider(t, js)

	require.NoError(t, second.ClaimEBSProvider(t.Context(), "node2", config.EBSProviderQEMUNBD))
	require.NoError(t, first.ClaimEBSProvider(t.Context(), "node1", config.EBSProviderQEMUNBD), "a restart on the same provider must start")

	rec, revAfter := recordedEBSProvider(t, js)
	assert.Equal(t, EBSProviderRecord{Provider: config.EBSProviderQEMUNBD, RecordedBy: "node1"}, rec)
	assert.Equal(t, rev, revAfter, "a matching claim must not rewrite the key")
}

func TestClaimEBSProvider_DifferentProviderRefused(t *testing.T) {
	ns, _, js := testutil.StartTestJetStream(t)
	first := newLockManager(t, ns.ClientURL())
	second := newLockManager(t, ns.ClientURL())

	require.NoError(t, first.ClaimEBSProvider(t.Context(), "node1", config.EBSProviderViperblockd))
	err := second.ClaimEBSProvider(t.Context(), "node2", config.EBSProviderQEMUNBD)

	var mismatch *EBSProviderMismatchError
	require.ErrorAs(t, err, &mismatch)
	assert.Equal(t, "node2", mismatch.Node)
	assert.Equal(t, config.EBSProviderQEMUNBD, mismatch.Configured)
	assert.Equal(t, EBSProviderRecord{Provider: config.EBSProviderViperblockd, RecordedBy: "node1"}, mismatch.Recorded)
	for _, want := range []string{`"qemunbd"`, `"viperblockd"`, "node1", ClusterConfigBucket, EBSProviderKey, `[nodes.node2.ebs] provider = "viperblockd"`, "wiping and reforming"} {
		assert.Contains(t, err.Error(), want)
	}

	rec, _ := recordedEBSProvider(t, js)
	assert.Equal(t, config.EBSProviderViperblockd, rec.Provider, "a refused claim must leave the recorded provider in place")
}

// Two daemons on different providers starting together: whichever Create wins
// is recorded, and the other is refused.
func TestClaimEBSProvider_ConcurrentDifferentProvidersLeaveOneKeyAndOneRefusal(t *testing.T) {
	for range 20 {
		ns, _, js := testutil.StartTestJetStream(t)
		managers := []*JetStreamManager{newLockManager(t, ns.ClientURL()), newLockManager(t, ns.ClientURL())}
		providers := []string{config.EBSProviderViperblockd, config.EBSProviderQEMUNBD}
		nodes := []string{"node1", "node2"}

		errs := make([]error, 2)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range managers {
			wg.Go(func() {
				<-start
				errs[i] = managers[i].ClaimEBSProvider(context.Background(), nodes[i], providers[i])
			})
		}
		close(start)
		wg.Wait()

		rec, _ := recordedEBSProvider(t, js)
		winner := slices.Index(providers, rec.Provider)
		require.GreaterOrEqual(t, winner, 0, "recorded provider %q is neither claimant's", rec.Provider)
		assert.Equal(t, nodes[winner], rec.RecordedBy)
		require.NoError(t, errs[winner])

		var mismatch *EBSProviderMismatchError
		require.ErrorAs(t, errs[1-winner], &mismatch, "loser must be refused")
		assert.Equal(t, rec.Provider, mismatch.Recorded.Provider)

		kv, err := js.KeyValue(t.Context(), ClusterConfigBucket)
		require.NoError(t, err)
		hist, err := kv.History(t.Context(), EBSProviderKey)
		require.NoError(t, err)
		assert.Len(t, hist, 1, "the key must be written exactly once")
	}
}

// The daemon refuses on the first mismatch rather than retrying it for the
// startup budget, and an unset provider resolves to viperblockd for the check.
func TestDaemonClaimEBSProvider_MismatchRefusesWithoutRetry(t *testing.T) {
	sleeps := stubInitRetrySleep(t)
	ns, _, _ := testutil.StartTestJetStream(t)

	first := &Daemon{node: "node1", config: &config.Config{EBS: config.EBSConfig{Provider: config.EBSProviderQEMUNBD}}, jsManager: newLockManager(t, ns.ClientURL())}
	require.NoError(t, first.claimEBSProvider())

	second := &Daemon{node: "node2", config: &config.Config{}, jsManager: newLockManager(t, ns.ClientURL())}
	err := second.claimEBSProvider()
	var mismatch *EBSProviderMismatchError
	require.ErrorAs(t, err, &mismatch)
	assert.Equal(t, config.EBSProviderViperblockd, mismatch.Configured)
	assert.Empty(t, *sleeps, "a mismatch must not be retried")
}

// A transient KV failure is retried until the bucket answers.
func TestDaemonClaimEBSProvider_RetriesUntilBucketOpens(t *testing.T) {
	ns, _, js := testutil.StartTestJetStream(t)
	m := newLockManager(t, ns.ClientURL())
	lock := m.ebsLock
	m.ebsLock = nil

	sleeps := 0
	prev := initRetrySleep
	initRetrySleep = func(time.Duration) {
		sleeps++
		m.ebsLock = lock
	}
	t.Cleanup(func() { initRetrySleep = prev })

	d := &Daemon{node: "node1", config: &config.Config{}, jsManager: m}
	require.NoError(t, d.claimEBSProvider())
	assert.Equal(t, 1, sleeps)
	rec, _ := recordedEBSProvider(t, js)
	assert.Equal(t, config.EBSProviderViperblockd, rec.Provider)
}
