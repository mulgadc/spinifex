package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
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

	rev, err := first.ClaimEBSProvider(t.Context(), "node1", config.EBSProviderQEMUNBD)
	require.NoError(t, err)

	joined, err := second.ClaimEBSProvider(t.Context(), "node2", config.EBSProviderQEMUNBD)
	require.NoError(t, err)
	assert.Equal(t, rev, joined)
	restarted, err := first.ClaimEBSProvider(t.Context(), "node1", config.EBSProviderQEMUNBD)
	require.NoError(t, err, "a restart on the same provider must start")
	assert.Equal(t, rev, restarted)

	rec, revAfter := recordedEBSProvider(t, js)
	assert.Equal(t, EBSProviderRecord{Provider: config.EBSProviderQEMUNBD, RecordedBy: "node1"}, rec)
	assert.Equal(t, rev, revAfter, "a matching claim must not rewrite the key")
}

func TestClaimEBSProvider_DifferentProviderRefused(t *testing.T) {
	ns, _, js := testutil.StartTestJetStream(t)
	first := newLockManager(t, ns.ClientURL())
	second := newLockManager(t, ns.ClientURL())

	_, err := first.ClaimEBSProvider(t.Context(), "node1", config.EBSProviderViperblockd)
	require.NoError(t, err)
	_, err = second.ClaimEBSProvider(t.Context(), "node2", config.EBSProviderQEMUNBD)

	var mismatch *EBSProviderMismatchError
	require.ErrorAs(t, err, &mismatch)
	assert.ErrorIs(t, err, errStartupPermanent)
	assert.Equal(t, "node2", mismatch.Node)
	assert.Equal(t, config.EBSProviderQEMUNBD, mismatch.Configured)
	assert.Equal(t, EBSProviderRecord{Provider: config.EBSProviderViperblockd, RecordedBy: "node1"}, mismatch.Recorded)
	for _, want := range []string{`"qemunbd"`, `"viperblockd"`, "node1", ClusterConfigBucket, EBSProviderKey, `[nodes.node2.ebs] provider = "viperblockd"`, "wiping and reforming"} {
		assert.Contains(t, err.Error(), want)
	}

	rec, _ := recordedEBSProvider(t, js)
	assert.Equal(t, config.EBSProviderViperblockd, rec.Provider, "a refused claim must leave the recorded provider in place")
}

// A value that does not decode, or names no supported provider, is refused as
// unreadable rather than as a mismatch, and quotes what the key holds.
func TestClaimEBSProvider_InvalidRecordRefused(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		reason string
	}{
		{"not JSON", "qemunbd", "not a JSON record"},
		{"empty provider", `{"provider":"","recorded_by":"node1"}`, `provider "" is not a supported provider`},
		{"unknown provider", `{"provider":"cephd","recorded_by":"node1"}`, `provider "cephd" is not a supported provider`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns, _, js := testutil.StartTestJetStream(t)
			m := newLockManager(t, ns.ClientURL())
			kv, err := js.KeyValue(t.Context(), ClusterConfigBucket)
			require.NoError(t, err)
			_, err = kv.Put(t.Context(), EBSProviderKey, []byte(tc.raw))
			require.NoError(t, err)

			_, err = m.ClaimEBSProvider(t.Context(), "node2", config.EBSProviderViperblockd)
			var invalid *EBSProviderRecordInvalidError
			require.ErrorAs(t, err, &invalid)
			assert.ErrorIs(t, err, errStartupPermanent)
			assert.NotErrorAs(t, err, new(*EBSProviderMismatchError))
			for _, want := range []string{ClusterConfigBucket, EBSProviderKey, strconv.Quote(tc.raw), tc.reason} {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
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
		revs := make([]uint64, 2)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range managers {
			wg.Go(func() {
				<-start
				revs[i], errs[i] = managers[i].ClaimEBSProvider(context.Background(), nodes[i], providers[i])
			})
		}
		close(start)
		wg.Wait()

		rec, rev := recordedEBSProvider(t, js)
		winner := slices.Index(providers, rec.Provider)
		require.GreaterOrEqual(t, winner, 0, "recorded provider %q is neither claimant's", rec.Provider)
		assert.Equal(t, nodes[winner], rec.RecordedBy)
		require.NoError(t, errs[winner])
		assert.Equal(t, revs[winner], rev, "the key must still be the winner's Create")

		var mismatch *EBSProviderMismatchError
		require.ErrorAs(t, errs[1-winner], &mismatch, "loser must be refused")
		assert.Equal(t, rec.Provider, mismatch.Recorded.Provider)
	}
}

// A permanent refusal returns on the first attempt rather than being retried
// for the startup budget.
func TestInitServiceWithRetry_PermanentErrorNotRetried(t *testing.T) {
	stubInitRetrySleep(t)
	calls := 0
	_, err := initServiceWithRetry("svc", func() (int, error) {
		calls++
		if calls > 1 {
			t.Fatal("a permanent error was retried")
		}
		return 0, fmt.Errorf("refused: %w", errStartupPermanent)
	})
	require.ErrorIs(t, err, errStartupPermanent)
	assert.Equal(t, 1, calls)
}

// An unset provider resolves to viperblockd for the check, and the daemon
// refuses on the first mismatch: a retry would need a sleep first.
func TestDaemonClaimEBSProvider_MismatchRefusesWithoutRetry(t *testing.T) {
	ns, _, _ := testutil.StartTestJetStream(t)
	prev := initRetrySleep
	initRetrySleep = func(time.Duration) { t.Fatal("a mismatch was retried") }
	t.Cleanup(func() { initRetrySleep = prev })

	first := &Daemon{node: "node1", config: &config.Config{EBS: config.EBSConfig{Provider: config.EBSProviderQEMUNBD}}, jsManager: newLockManager(t, ns.ClientURL())}
	require.NoError(t, first.claimEBSProvider())

	second := &Daemon{node: "node2", config: &config.Config{}, jsManager: newLockManager(t, ns.ClientURL())}
	err := second.claimEBSProvider()
	var mismatch *EBSProviderMismatchError
	require.ErrorAs(t, err, &mismatch)
	assert.Equal(t, config.EBSProviderViperblockd, mismatch.Configured)
}

// A transient KV failure is retried until the bucket answers.
func TestDaemonClaimEBSProvider_RetriesUntilBucketOpens(t *testing.T) {
	ns, _, js := testutil.StartTestJetStream(t)
	m := newLockManager(t, ns.ClientURL())
	bucket := m.clusterConfig
	m.clusterConfig = nil

	sleeps := 0
	prev := initRetrySleep
	initRetrySleep = func(time.Duration) {
		sleeps++
		m.clusterConfig = bucket
	}
	t.Cleanup(func() { initRetrySleep = prev })

	d := &Daemon{node: "node1", config: &config.Config{}, jsManager: m}
	require.NoError(t, d.claimEBSProvider())
	assert.Equal(t, 1, sleeps)
	rec, _ := recordedEBSProvider(t, js)
	assert.Equal(t, config.EBSProviderViperblockd, rec.Provider)
}

// A daemon whose provider differs from the recorded one exits 1 before any
// cluster service is built or any instance restored.
func TestStartCluster_EBSProviderMismatchExits(t *testing.T) {
	t.Setenv("SPINIFEX_REQUIRE_NATS", "")
	ns, _, js := testutil.StartTestJetStream(t)
	seed := newLockManager(t, ns.ClientURL())
	_, err := seed.ClaimEBSProvider(t.Context(), "node-2", config.EBSProviderQEMUNBD)
	require.NoError(t, err)

	tmpDir := t.TempDir()
	cfg := &config.ClusterConfig{
		Node: "node-1",
		Nodes: map[string]config.Config{
			"node-1": {BaseDir: tmpDir, DataDir: tmpDir, NATS: config.NATSConfig{Host: ns.ClientURL()}},
		},
	}
	d, err := NewDaemon(cfg)
	require.NoError(t, err)
	t.Cleanup(d.cancel)
	exitCh := make(chan int, 1)
	d.exitFunc = func(code int) { exitCh <- code }

	err = d.startCluster()
	var mismatch *EBSProviderMismatchError
	require.ErrorAs(t, err, &mismatch)
	assert.Equal(t, config.EBSProviderViperblockd, mismatch.Configured)
	select {
	case code := <-exitCh:
		assert.Equal(t, 1, code)
	default:
		t.Fatal("a provider mismatch must request exit(1)")
	}

	assert.Nil(t, d.instanceService, "no cluster service may be built after the refusal")
	assert.Nil(t, d.imageService)
	assert.Nil(t, d.snapshotService)
	assert.Nil(t, d.volumeService)
	assert.Nil(t, d.vpcService)
	assert.Nil(t, d.eksService)
	assert.Zero(t, d.vmMgr.Count(), "no instance may be restored after the refusal")

	rec, _ := recordedEBSProvider(t, js)
	assert.Equal(t, config.EBSProviderQEMUNBD, rec.Provider)
}
