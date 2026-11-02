package kvstore

import (
	"context"
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type resetRecord struct {
	N int `json:"n"`
}

// replayWatcher hands out a fixed sequence of deliveries, as an ordered
// consumer that reset partway and restarted from an earlier sequence would.
type replayWatcher struct{ updates chan jetstream.KeyValueEntry }

func (w *replayWatcher) Updates() <-chan jetstream.KeyValueEntry { return w.updates }
func (w *replayWatcher) Stop() error                             { return nil }

// resettingKV delivers every initial entry twice, with an older revision of
// the first key in between, and only then the watcher's own marker.
type resettingKV struct {
	jetstream.KeyValue

	stale jetstream.KeyValueEntry
}

func (r resettingKV) Watch(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	inner, err := r.KeyValue.Watch(ctx, keys, opts...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = inner.Stop() }()
	var initial []jetstream.KeyValueEntry
	for entry := range inner.Updates() {
		if entry == nil {
			break
		}
		initial = append(initial, entry)
	}
	w := &replayWatcher{updates: make(chan jetstream.KeyValueEntry, 2*len(initial)+2)}
	for _, e := range initial {
		w.updates <- e
	}
	w.updates <- r.stale
	for _, e := range initial {
		w.updates <- e
	}
	w.updates <- nil
	return w, nil
}

func TestAReadAcrossAConsumerResetReturnsEachKeyOnceAtItsLatestRevision(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	ctx := t.Context()
	store := New[resetRecord](testutil.NewJetStream(t, nc), Config{Name: "read-reset", History: 5})
	require.NoError(t, store.Set(ctx, "a", &resetRecord{N: 1}))
	kv, err := store.KV(ctx)
	require.NoError(t, err)
	stale, err := kv.Get(ctx, "a")
	require.NoError(t, err)
	require.NoError(t, store.Set(ctx, "a", &resetRecord{N: 2}))
	require.NoError(t, store.Set(ctx, "b", &resetRecord{N: 3}))

	latest, mark, err := store.readRange(ctx, resettingKV{KeyValue: kv, stale: stale}, ">", false)
	require.NoError(t, err)

	require.Len(t, latest, 2)
	assert.JSONEq(t, `{"n":2}`, string(latest["a"].Value()), "the stale revision replayed after the reset does not win")
	assert.JSONEq(t, `{"n":3}`, string(latest["b"].Value()))
	assert.Equal(t, latest["b"].Revision(), mark)
}
