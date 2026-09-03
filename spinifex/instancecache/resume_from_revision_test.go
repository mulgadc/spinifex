package instancecache_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestResumeFromRevision_ReplaysWhatTheSnapshotMissed documents the server
// behaviour the whole Cache design rests on: a watcher opened AFTER a
// snapshot, resuming from just past the snapshot's high-water mark, replays
// exactly the events the snapshot missed — even though nothing was watching
// while they happened — and nothing the snapshot already covered. If a
// nats.go upgrade ever changes this, this test fails loudly and points
// straight at the cause, rather than the cache's own tests failing in a way
// that looks like a cache bug.
func TestResumeFromRevision_ReplaysWhatTheSnapshotMissed(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	ctx := context.Background()

	store := kvstore.New[vm.InstanceRecord](js, kvstore.Config{Name: "resume-bucket", History: 1, Replicas: 1})

	rec := func(id string) *vm.InstanceRecord {
		r := &vm.InstanceRecord{}
		r.Metadata.Name = id
		return r
	}

	// Three keys exist before the snapshot.
	for i := range 3 {
		require.NoError(t, store.Set(ctx, fmt.Sprintf("i.pre-%d", i), rec(fmt.Sprintf("pre-%d", i))))
	}

	items, highWater, err := store.Snapshot(ctx, "i.*")
	require.NoError(t, err)
	require.Len(t, items, 3, "snapshot should see the three pre-existing keys")

	// These land AFTER the snapshot and BEFORE any watcher exists. This is
	// exactly the window a buffering fence would otherwise have to cover.
	require.NoError(t, store.Set(ctx, "i.post-1", rec("post-1")))
	require.NoError(t, store.Delete(ctx, "i.pre-0"))

	// Open the watcher only now, resuming from just past the snapshot.
	kw, err := js.KeyValue(ctx, "resume-bucket")
	require.NoError(t, err)
	w, err := kw.Watch(ctx, "i.*", jetstream.ResumeFromRevision(highWater+1))
	require.NoError(t, err)
	defer func() { _ = w.Stop() }()

	seen := map[string]jetstream.KeyValueOp{}
	timeout := time.After(10 * time.Second)
	for {
		select {
		case e := <-w.Updates():
			if e == nil {
				// End-of-replay marker: everything the resume owed us has arrived.
				require.Equal(t, jetstream.KeyValuePut, seen["i.post-1"],
					"a put made after the snapshot, before the watcher existed, must be replayed")
				require.Equal(t, jetstream.KeyValueDelete, seen["i.pre-0"],
					"a delete made after the snapshot, before the watcher existed, must be replayed")
				require.NotContains(t, seen, "i.pre-1", "keys already in the snapshot must not be replayed")
				return
			}
			seen[e.Key()] = e.Operation()
		case <-timeout:
			t.Fatalf("no end-of-replay marker within 10s; saw %v", seen)
		}
	}
}
