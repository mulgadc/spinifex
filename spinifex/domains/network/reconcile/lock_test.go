package reconcile

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/clustersize"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvsync"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// vpcdLease returns holder's vpcd lease on a store of its own, standing in for
// a separate process.
func vpcdLease(js jetstream.JetStream, holder string) *kvsync.Lease {
	return NewVPCDLease(kvstore.OpenSyncBucket(js), holder)
}

func TestAcquireLeader_OneHolderAtATimeAcrossProcesses(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	node1, node2 := vpcdLease(js, "node-1"), vpcdLease(js, "node-2")

	release, ok := AcquireLeader(t.Context(), node1)
	require.True(t, ok, "the first acquire on a free key must win")

	loserRelease, ok := AcquireLeader(t.Context(), node2)
	assert.False(t, ok, "a second process must lose while the lease is held")
	assert.Nil(t, loserRelease)

	release()
	release2, ok := AcquireLeader(t.Context(), node2)
	require.True(t, ok, "acquire after release must win at once")
	release2()
}

// The two vpcd sites share one Lease, so they exclude each other on one node
// as well as across nodes.
func TestAcquireLeader_SitesSharingALeaseExcludeEachOther(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	shared := vpcdLease(js, "node-1")

	startup, ok := AcquireLeader(t.Context(), shared)
	require.True(t, ok)

	_, ok = AcquireLeader(t.Context(), shared)
	assert.False(t, ok, "the drift pass ran while the startup pass held the lease")

	startup()
	drift, ok := AcquireLeader(t.Context(), shared)
	require.True(t, ok, "the drift pass must run once the startup pass has released")
	drift()
}

// Shutdown cancels the acquiring context first and then releases, so a release
// bound to that context would no-op and leave the next leader waiting a TTL.
func TestAcquireLeader_ReleaseSurvivesCancelledContext(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	node1, node2 := vpcdLease(js, "node-1"), vpcdLease(js, "node-2")

	ctx, cancel := context.WithCancel(t.Context())
	release, ok := AcquireLeader(ctx, node1)
	require.True(t, ok)
	cancel()
	release()

	release2, ok := AcquireLeader(t.Context(), node2)
	require.True(t, ok, "the lease must be free immediately after release")
	release2()
}

// A losing acquire logs who holds the key, not the local node as holder.
func TestAcquireLeader_NotLeaderLogNamesTheHolder(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	node1, node2 := vpcdLease(js, "node-1"), vpcdLease(js, "node-2")

	release, ok := AcquireLeader(t.Context(), node1)
	require.True(t, ok)
	defer release()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	_, ok = AcquireLeader(t.Context(), node2)
	require.False(t, ok)

	var line map[string]any
	dec := json.NewDecoder(&buf)
	for dec.More() {
		var rec map[string]any
		require.NoError(t, dec.Decode(&rec))
		if rec["msg"] == "reconcile/lock: not leader, skipping reconcile" {
			line = rec
		}
	}
	require.NotNil(t, line, "a losing acquire must log that it is skipping")
	assert.Equal(t, "node-1", line["holder"])
	assert.Equal(t, KeyVPCDReconcile, line["key"])
}

// coldStore answers every Load with ErrUnavailable until down reaches zero,
// standing in for a JetStream still forming quorum.
type coldStore struct {
	kvsync.KVStore

	down atomic.Int32
}

func (c *coldStore) Load(ctx context.Context, key string) ([]byte, uint64, error) {
	if c.down.Add(-1) >= 0 {
		return nil, 0, kvsync.ErrUnavailable
	}
	return c.KVStore.Load(ctx, key)
}

func shrinkStartRetry(t *testing.T, retryFor, step time.Duration) {
	t.Helper()
	prevFor, prevStep := startRetryFor, startRetryStep
	startRetryFor, startRetryStep = retryFor, step
	t.Cleanup(func() { startRetryFor, startRetryStep = prevFor, prevStep })
}

// A pass that loses to a peer asks to run again within a renewal interval, so a
// follower that first saw a dead holder's revision sees it a TTL later and takes
// over about 4*TTL/3 after the last renewal rather than up to 2*TTL.
func TestElectPass_AHeldLeaseAsksForATakeoverRevisit(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	release, ok := AcquireLeader(t.Context(), vpcdLease(js, "node-1"))
	require.True(t, ok)
	defer release()

	_, revisit, ok := ElectPass(t.Context(), vpcdLease(js, "node-2"))
	assert.False(t, ok)
	assert.Positive(t, revisit, "a held pass must ask to run again, not wait for a resync")
	assert.LessOrEqual(t, revisit, LeaderTTL/3, "the retry is a renewal interval out, so takeover is not delayed a further TTL")
}

func TestElectPass_AWonPassAsksForNoRevisit(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	release, revisit, ok := ElectPass(t.Context(), vpcdLease(js, "node-1"))
	require.True(t, ok)
	release()
	assert.Zero(t, revisit)
}

// A cold multi-node start has no quorum yet, and the one startup election
// must wait for it rather than skip the startup reconcile.
func TestAcquireLeaderAtStart_WaitsOutAnUnavailableStore(t *testing.T) {
	shrinkStartRetry(t, 10*time.Second, 5*time.Millisecond)
	_, _, js := testutil.StartTestJetStream(t)
	cold := &coldStore{KVStore: kvstore.OpenSyncBucket(js)}
	cold.down.Store(3)

	release, ok := AcquireLeaderAtStart(t.Context(), NewVPCDLease(cold, "node-1"))
	require.True(t, ok, "the election must be won once the store answers")
	release()
}

// formingJS fails its first down bucket lookups and creates with err, as a
// cluster still electing its meta leader or placing replicas does.
type formingJS struct {
	jetstream.JetStream

	err  error
	down atomic.Int32
}

func (f *formingJS) KeyValue(ctx context.Context, name string) (jetstream.KeyValue, error) {
	if f.down.Add(-1) >= 0 {
		return nil, f.err
	}
	return f.JetStream.KeyValue(ctx, name)
}

func (f *formingJS) CreateKeyValue(ctx context.Context, cfg jetstream.KeyValueConfig) (jetstream.KeyValue, error) {
	if f.down.Add(-1) >= 0 {
		return nil, f.err
	}
	return f.JetStream.CreateKeyValue(ctx, cfg)
}

// The startup election waits out each shape a forming cluster fails in, as the
// real bucket reports it, not only the errors already classed as unavailable.
func TestAcquireLeaderAtStart_WaitsOutAFormingCluster(t *testing.T) {
	cases := map[string]error{
		"no meta leader":       &jetstream.APIError{Code: 503, ErrorCode: 10008, Description: "JetStream system temporarily unavailable"},
		"no suitable peers":    &jetstream.APIError{Code: 400, ErrorCode: 10005, Description: "no suitable peers for placement"},
		"insufficient storage": &jetstream.APIError{Code: 503, ErrorCode: 10023, Description: "insufficient resources"},
	}
	for name, failure := range cases {
		t.Run(name, func(t *testing.T) {
			shrinkStartRetry(t, 10*time.Second, 5*time.Millisecond)
			_, _, js := testutil.StartTestJetStream(t)
			forming := &formingJS{JetStream: js, err: failure}
			forming.down.Store(4)

			release, ok := AcquireLeaderAtStart(t.Context(), NewVPCDLease(kvstore.OpenSyncBucket(forming), "node-1"))
			require.True(t, ok, "the election must be won once the cluster has formed")
			release()
		})
	}
}

func TestAcquireLeaderAtStart_GivesUpWhenTheWaitRunsOut(t *testing.T) {
	shrinkStartRetry(t, 50*time.Millisecond, 5*time.Millisecond)
	_, _, js := testutil.StartTestJetStream(t)
	cold := &coldStore{KVStore: kvstore.OpenSyncBucket(js)}
	cold.down.Store(1 << 30)

	start := time.Now()
	_, ok := AcquireLeaderAtStart(t.Context(), NewVPCDLease(cold, "node-1"))
	assert.False(t, ok)
	assert.Less(t, time.Since(start), 5*time.Second)
}

// A held lease is an answer, not an outage, so it is not waited on.
func TestAcquireLeaderAtStart_AHeldLeaseAnswersAtOnce(t *testing.T) {
	shrinkStartRetry(t, time.Minute, time.Second)
	_, _, js := testutil.StartTestJetStream(t)
	release, ok := AcquireLeader(t.Context(), vpcdLease(js, "node-1"))
	require.True(t, ok)
	defer release()

	start := time.Now()
	_, ok = AcquireLeaderAtStart(t.Context(), vpcdLease(js, "node-2"))
	assert.False(t, ok)
	assert.Less(t, time.Since(start), 5*time.Second, "a held lease must not consume the retry window")
}

// An undeclared cluster size never becomes declared by waiting, so it ends the
// wait at once instead of holding the startup reconcile for the whole window.
func TestAcquireLeaderAtStart_StopsOnAConfigFault(t *testing.T) {
	shrinkStartRetry(t, time.Minute, time.Second)
	_, _, js := testutil.StartTestJetStream(t)
	clustersize.RedeclareForTest(t, 0)

	start := time.Now()
	_, ok := AcquireLeaderAtStart(t.Context(), vpcdLease(js, "node-1"))
	assert.False(t, ok)
	assert.Less(t, time.Since(start), 10*time.Second, "a permanent fault must not consume the retry window")
}
