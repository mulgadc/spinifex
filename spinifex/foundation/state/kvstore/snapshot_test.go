package kvstore_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

type snapRecord struct {
	Name string `json:"name"`
}

func snapshotStore(t *testing.T, name string) *kvstore.Store[snapRecord] {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	return kvstore.New[snapRecord](testutil.NewJetStream(t, nc), kvstore.Config{Name: name, History: 1})
}

func TestSnapshotReturnsMatchingRecordsWithTheirRevisions(t *testing.T) {
	t.Parallel()
	store := snapshotStore(t, "snapshot-records")
	require.NoError(t, store.Set(t.Context(), "i.a", &snapRecord{Name: "a"}))
	require.NoError(t, store.Set(t.Context(), "i.b", &snapRecord{Name: "b"}))
	require.NoError(t, store.Set(t.Context(), "node.1", &snapRecord{Name: "elsewhere"}))

	items, highWater, err := store.Snapshot(t.Context(), "i.*")
	require.NoError(t, err)
	require.Len(t, items, 2)

	byKey := map[string]kvstore.Item[snapRecord]{}
	for _, item := range items {
		byKey[item.Key] = item
		require.NotZero(t, item.Revision)
	}
	require.Equal(t, "a", byKey["i.a"].Value.Name)
	require.Equal(t, "b", byKey["i.b"].Value.Name)
	require.NotContains(t, byKey, "node.1")
	require.Equal(t, byKey["i.b"].Revision, highWater)
}

// The high-water mark tracks the reader's position in the stream, not what
// survives in it. A delete is a message, so a snapshot whose newest event is a
// delete still reaches the watermark and can still lower a counter.
func TestSnapshotCountsATombstoneTowardsTheHighWaterMark(t *testing.T) {
	t.Parallel()
	store := snapshotStore(t, "snapshot-tombstone")
	require.NoError(t, store.Set(t.Context(), "i.a", &snapRecord{Name: "a"}))
	require.NoError(t, store.Set(t.Context(), "i.b", &snapRecord{Name: "b"}))
	require.NoError(t, store.Delete(t.Context(), "i.b"))

	items, highWater, err := store.Snapshot(t.Context(), "i.*")
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, "i.a", items[0].Key)

	watermark, err := store.LastSequence(t.Context(), "i.*")
	require.NoError(t, err)
	require.Equal(t, watermark, highWater)
	require.Greater(t, highWater, items[0].Revision)
}

func TestSnapshotOfNothingIsNotAnError(t *testing.T) {
	t.Parallel()
	store := snapshotStore(t, "snapshot-empty")
	require.NoError(t, store.Set(t.Context(), "node.1", &snapRecord{Name: "elsewhere"}))

	items, highWater, err := store.Snapshot(t.Context(), "i.*")
	require.NoError(t, err)
	require.Empty(t, items)
	require.Zero(t, highWater)
}

func TestLastSequenceIsTheNewestMessageMatchingTheFilter(t *testing.T) {
	t.Parallel()
	store := snapshotStore(t, "watermark-newest")
	require.NoError(t, store.Set(t.Context(), "i.a", &snapRecord{Name: "a"}))
	items, highWater, err := store.Snapshot(t.Context(), "i.*")
	require.NoError(t, err)
	require.Len(t, items, 1)

	watermark, err := store.LastSequence(t.Context(), "i.*")
	require.NoError(t, err)
	require.Equal(t, highWater, watermark)

	// A write outside the filter must not move it, which is what makes the
	// mark comparable to a snapshot of the same filter.
	require.NoError(t, store.Set(t.Context(), "node.1", &snapRecord{Name: "elsewhere"}))
	after, err := store.LastSequence(t.Context(), "i.*")
	require.NoError(t, err)
	require.Equal(t, watermark, after)
}

func TestLastSequenceIsZeroWhenNothingMatches(t *testing.T) {
	t.Parallel()
	store := snapshotStore(t, "watermark-empty")
	require.NoError(t, store.Set(t.Context(), "node.1", &snapRecord{Name: "elsewhere"}))

	watermark, err := store.LastSequence(t.Context(), "i.*")
	require.NoError(t, err)
	require.Zero(t, watermark)
}

// A client reaching JetStream through an imported API prefix sends the
// leader read under that prefix; its own account has no JetStream to answer
// the default one.
func TestLastSequenceHonoursTheAPIPrefix(t *testing.T) {
	t.Parallel()
	conf := filepath.Join(t.TempDir(), "server.conf")
	require.NoError(t, os.WriteFile(conf, []byte(`
listen: "127.0.0.1:-1"
jetstream: {store_dir: "`+t.TempDir()+`"}
accounts: {
  JS: {jetstream: enabled, users: [{user: js, password: js}], exports: [{service: "$JS.API.>"}]}
  APP: {users: [{user: app, password: app}], imports: [{service: {account: JS, subject: "$JS.API.>"}, to: "pre.API.>"}]}
}
`), 0o600))
	opts, err := server.ProcessConfigFile(conf)
	require.NoError(t, err)
	opts.NoLog, opts.NoSigs = true, true
	srv, err := server.NewServer(opts)
	require.NoError(t, err)
	go srv.Start()
	require.True(t, srv.ReadyForConnections(10*time.Second))
	t.Cleanup(srv.Shutdown)

	connect := func(user string) *nats.Conn {
		nc, err := nats.Connect(srv.ClientURL(), nats.UserInfo(user, user))
		require.NoError(t, err)
		t.Cleanup(nc.Close)
		return nc
	}
	owner := kvstore.New[snapRecord](testutil.NewJetStream(t, connect("js")), kvstore.Config{Name: "watermark-prefix", History: 1})
	require.NoError(t, owner.Set(t.Context(), "i.a", &snapRecord{Name: "a"}))
	want, err := owner.LastSequence(t.Context(), "i.*")
	require.NoError(t, err)
	require.NotZero(t, want)

	js, err := jetstream.NewWithAPIPrefix(connect("app"), "pre.API")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	got, err := kvstore.NewBucket(js, kvstore.Config{Name: "watermark-prefix"}).LastSequence(ctx, "i.*")
	require.NoError(t, err)
	require.Equal(t, want, got)
}
