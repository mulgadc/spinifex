package reconcile

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
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
