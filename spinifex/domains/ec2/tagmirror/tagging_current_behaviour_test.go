package tagmirror

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The TestCurrentBehaviour_Tagging_ tests pin how the EC2 record-side tag
// mirror writes resource records today. They describe current behaviour, not
// the target design, and a failure means the behaviour changed.

// interleavingKV runs between once, after the first read of key returns and
// before the caller can write, standing in for a writer on another node.
type interleavingKV struct {
	jetstream.KeyValue

	key     string
	once    sync.Once
	between func()
}

func (k *interleavingKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	entry, err := k.KeyValue.Get(ctx, key)
	if key == k.key {
		k.once.Do(k.between)
	}
	return entry, err
}

// UpdateKVRecordTags commits at the revision it read and retries on conflict,
// so a tag write and a non-tag record change that commit between its read and
// its write both survive alongside its own tag.
func TestCurrentBehaviour_Tagging_RecordMirrorKeepsInterleavedWrites(t *testing.T) {
	const acct, id = "123456789012", "sg-0123456789abcdef0"
	key := kvutil.AccountKey(acct, id)
	seed, err := json.Marshal(tagMirrorRecord{Name: "original", Tags: map[string]string{}})
	require.NoError(t, err)
	kv := seedKV(t, "tagmirror-cb", map[string][]byte{key: seed})
	ctx := context.Background()

	wrapped := &interleavingKV{KeyValue: kv, key: key, between: func() {
		require.NoError(t, UpdateKVRecordTags(ctx, kv, acct, id, func(r *tagMirrorRecord) {
			r.Name = "renamed"
			r.Tags["b"] = "2"
		}))
	}}

	require.NoError(t, UpdateKVRecordTags(ctx, wrapped, acct, id, func(r *tagMirrorRecord) {
		r.Tags["a"] = "1"
	}))

	entry, err := kv.Get(ctx, key)
	require.NoError(t, err)
	var final tagMirrorRecord
	require.NoError(t, json.Unmarshal(entry.Value(), &final))
	assert.Equal(t, map[string]string{"a": "1", "b": "2"}, final.Tags, "both tag writes survive")
	assert.Equal(t, "renamed", final.Name, "the interleaved rename survives")
}
