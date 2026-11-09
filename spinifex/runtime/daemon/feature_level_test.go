package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/foundation/release/featurelevel"
	instancecache "github.com/mulgadc/spinifex/spinifex/runtime/compute/cache"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// levelManager opens both buckets on a server of its own, so the cluster-config
// record is not shared with parallel tests.
func levelManager(t *testing.T) (*JetStreamManager, *featurelevel.Store) {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	m, err := NewJetStreamManager(nc)
	require.NoError(t, err)
	require.NoError(t, m.InitClusterConfigBucket())
	require.NoError(t, m.InitClusterStateBucket())
	store, err := m.FeatureLevelStore(t.Context())
	require.NoError(t, err)
	return m, store
}

func levelBeat(t *testing.T, m *JetStreamManager, node string, level int, now time.Time, age time.Duration) {
	t.Helper()
	require.NoError(t, m.WriteHeartbeat(&Heartbeat{
		Node: node, Timestamp: now.Add(-age).UTC().Format(time.RFC3339), FeatureLevel: level,
	}))
}

func tick(t *testing.T, m *JetStreamManager, store *featurelevel.Store, nodes []string, now time.Time) (featurelevel.Record, bool) {
	t.Helper()
	rec, advanced, err := advanceFeatureLevel(t.Context(), store, m.ReadHeartbeat, nodes, now)
	require.NoError(t, err)
	return rec, advanced
}

func TestHeartbeatWithoutFeatureLevelReadsAsZeroAndBlocksAdvance(t *testing.T) {
	t.Parallel()
	m, store := levelManager(t)
	now := time.Now()
	// The shape an older build writes: no feature_level key at all.
	old := `{"node":"old","epoch":1,"timestamp":"` + now.UTC().Format(time.RFC3339) + `","services":["daemon"],"vm_count":0}`
	_, err := m.clusterKV.Put(t.Context(), clusterv1.HeartbeatKey("old"), []byte(old))
	require.NoError(t, err)
	h, err := m.ReadHeartbeat("old")
	require.NoError(t, err)
	assert.Zero(t, h.FeatureLevel)

	levelBeat(t, m, "new", featurelevel.Build, now, 0)
	rec, advanced := tick(t, m, store, []string{"new", "old"}, now)
	assert.False(t, advanced, "a member on an older build holds the cluster at 0")
	assert.Zero(t, rec.Level)
}

func TestFeatureLevelAdvancesOnceEveryMemberIsCurrent(t *testing.T) {
	t.Parallel()
	m, store := levelManager(t)
	now := time.Now()
	nodes := []string{"a", "b", "c"}
	levelBeat(t, m, "a", featurelevel.Build, now, 0)
	levelBeat(t, m, "b", featurelevel.Build, now, 0)

	rec, advanced := tick(t, m, store, nodes, now)
	assert.False(t, advanced, "member c has no heartbeat key (never written or expired from the 1h bucket)")
	assert.Zero(t, rec.Level)

	levelBeat(t, m, "c", featurelevel.Build, now, 0)
	rec, advanced = tick(t, m, store, nodes, now)
	assert.True(t, advanced)
	assert.Equal(t, featurelevel.Build, rec.Level)
}

func TestFeatureLevelStaleMemberBlocksAndLaterStalenessNeverLowers(t *testing.T) {
	t.Parallel()
	m, store := levelManager(t)
	now := time.Now()
	nodes := []string{"a", "b"}
	levelBeat(t, m, "a", featurelevel.Build, now, 0)
	levelBeat(t, m, "b", featurelevel.Build, now, instancecache.NodeStaleAfter+5*time.Second)

	rec, advanced := tick(t, m, store, nodes, now)
	assert.False(t, advanced, "a heartbeat older than NodeStaleAfter is not current")
	assert.Zero(t, rec.Level)

	levelBeat(t, m, "b", featurelevel.Build, now, instancecache.NodeStaleAfter-5*time.Second)
	rec, advanced = tick(t, m, store, nodes, now)
	assert.True(t, advanced)
	assert.Equal(t, featurelevel.Build, rec.Level)

	later := now.Add(10 * time.Minute)
	rec, advanced = tick(t, m, store, nodes, later)
	assert.False(t, advanced)
	assert.Equal(t, featurelevel.Build, rec.Level, "every member going stale afterwards does not lower the level")
}

func TestFeatureLevelReadErrorAdvancesNothing(t *testing.T) {
	t.Parallel()
	_, store := levelManager(t)
	boom := errors.New("kv unavailable")
	read := func(string) (*Heartbeat, error) { return nil, boom }
	rec, advanced, err := advanceFeatureLevel(t.Context(), store, read, []string{"a"}, time.Now())
	require.ErrorIs(t, err, boom)
	assert.False(t, advanced)
	assert.Zero(t, rec.Level)
	got, _, rerr := store.Read(t.Context())
	require.NoError(t, rerr)
	assert.Zero(t, got.Level)
}

func TestFeatureLevelUnparseableTimestampIsNotCurrent(t *testing.T) {
	t.Parallel()
	m, store := levelManager(t)
	require.NoError(t, m.WriteHeartbeat(&Heartbeat{Node: "a", Timestamp: "yesterday", FeatureLevel: featurelevel.Build}))
	_, advanced := tick(t, m, store, []string{"a"}, time.Now())
	assert.False(t, advanced)
}

func TestFeatureLevelStoreNeedsTheClusterConfigBucket(t *testing.T) {
	t.Parallel()
	_, nc, _ := testutil.StartTestJetStream(t)
	m, err := NewJetStreamManager(nc)
	require.NoError(t, err)
	_, err = m.FeatureLevelStore(context.Background())
	require.Error(t, err)
}

// flakyKV fails its first n Gets, then reads through.
type flakyKV struct {
	featurelevel.KV

	failures int
}

func (k *flakyKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	if k.failures > 0 {
		k.failures--
		return nil, errors.New("kv unavailable")
	}
	return k.KV.Get(ctx, key)
}

func recordLevel(t *testing.T, m *JetStreamManager, body string) {
	t.Helper()
	kv, err := m.clusterConfig.KV(t.Context())
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), featurelevel.Key, []byte(body))
	require.NoError(t, err)
}

func TestEnforceFeatureLevel_RefusesOnlyWhenTheBuildIsBelowTheRecord(t *testing.T) {
	t.Parallel()
	m, store := levelManager(t)

	require.NoError(t, enforceFeatureLevel(store), "an absent record is level 0")

	recordLevel(t, m, `{"level":1}`)
	require.NoError(t, enforceFeatureLevel(store), "equal starts")

	recordLevel(t, m, `{"level":2}`)
	err := enforceFeatureLevel(store)
	var refused *FeatureLevelRefusedError
	require.ErrorAs(t, err, &refused)
	require.ErrorIs(t, err, errStartupPermanent)
	assert.Equal(t, featurelevel.Build, refused.Build)
	assert.Equal(t, 2, refused.Recorded)
	assert.Contains(t, err.Error(), "spx admin cluster rollback --to 1")
	assert.Contains(t, err.Error(), "2")
}

func TestEnforceFeatureLevel_ReadFailureRetriesThenStartsWithoutRefusing(t *testing.T) {
	old := initRetrySleep
	initRetrySleep = func(time.Duration) {}
	t.Cleanup(func() { initRetrySleep = old })

	m, store := levelManager(t)
	_ = store
	recordLevel(t, m, `{"level":1}`)
	kv, err := m.clusterConfig.KV(t.Context())
	require.NoError(t, err)
	flaky := &flakyKV{KV: kv, failures: 3}
	require.NoError(t, enforceFeatureLevel(featurelevel.NewStore(flaky)))
	assert.Zero(t, flaky.failures, "it retried through every failure and then read the record")
}

func TestEnforceFeatureLevel_ReadFailureThenARefusingRecordStillRefuses(t *testing.T) {
	old := initRetrySleep
	initRetrySleep = func(time.Duration) {}
	t.Cleanup(func() { initRetrySleep = old })

	m, _ := levelManager(t)
	recordLevel(t, m, `{"level":3}`)
	kv, err := m.clusterConfig.KV(t.Context())
	require.NoError(t, err)
	err = enforceFeatureLevel(featurelevel.NewStore(&flakyKV{KV: kv, failures: 2}))
	require.ErrorIs(t, err, errStartupPermanent)
}

// A build below the recorded level exits 1 before the EBS provider is claimed
// or any cluster service is built.
func TestStartCluster_BuildBelowFeatureLevelExitsBeforeAnyClaim(t *testing.T) {
	t.Setenv("SPINIFEX_REQUIRE_NATS", "")
	ns, _, js := testutil.StartTestJetStream(t)
	seed := newLockManager(t, ns.ClientURL())
	recordLevel(t, seed, `{"level":2}`)

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
	var refused *FeatureLevelRefusedError
	require.ErrorAs(t, err, &refused)
	select {
	case code := <-exitCh:
		assert.Equal(t, 1, code)
	default:
		t.Fatal("a build below the recorded level must request exit(1)")
	}

	kv, kerr := js.KeyValue(t.Context(), ClusterConfigBucket)
	require.NoError(t, kerr)
	_, gerr := kv.Get(t.Context(), EBSProviderKey)
	require.ErrorIs(t, gerr, jetstream.ErrKeyNotFound, "claimEBSProvider must not have been reached")
	assert.Nil(t, d.instanceService)
	assert.Zero(t, d.vmMgr.Count())
}

// A node that has not set its gate must not answer the status route at all: a
// reply would read as nothing in flight to the rollback barrier.
func TestFeatureLevelStatusSubs_NilGateServesNothing(t *testing.T) {
	t.Parallel()
	d := &Daemon{node: "node-1"}
	assert.Empty(t, d.featureLevelStatusSubs())

	_, store := levelManager(t)
	d.featureGate = featurelevel.NewGate(store)
	subs := d.featureLevelStatusSubs()
	require.Len(t, subs, 1)
	assert.Equal(t, featurelevel.StatusSubject("node-1"), subs[0].topic)
	assert.Empty(t, subs[0].queueGroup)

	d.node = "a.b"
	assert.Empty(t, d.featureLevelStatusSubs(), "an invalid node id serves nothing")
}
