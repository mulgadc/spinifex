package elbv2

import (
	"testing"
	"time"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFixtureLeaderGet_ExactValueRevisionAndAbsence(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: KVBucketELBv2, History: 1})
	require.NoError(t, err)
	get := leaderGetOf(js, KVBucketELBv2)
	ctx := t.Context()

	rev, err := kv.Put(ctx, "live", []byte(`{"a":1}`))
	require.NoError(t, err)
	sub, err := nc.SubscribeSync("$JS.API.>")
	require.NoError(t, err)
	e, err := get(ctx, "live")
	require.NoError(t, err)
	assert.Equal(t, []byte(`{"a":1}`), e.Value())
	assert.Equal(t, rev, e.Revision(), "the revision is the CAS precondition")
	assert.Equal(t, "live", e.Key())
	require.NoError(t, nc.Flush())
	msg, err := sub.NextMsg(time.Second)
	require.NoError(t, err)
	assert.Equal(t, "$JS.API.STREAM.MSG.GET.KV_"+KVBucketELBv2, msg.Subject, "a message get, which only the stream leader answers")
	require.NoError(t, sub.Unsubscribe())

	for _, key := range []string{"deleted", "purged"} {
		_, err := kv.Put(ctx, key, []byte(`{}`))
		require.NoError(t, err)
	}
	require.NoError(t, kv.Delete(ctx, "deleted"))
	require.NoError(t, kv.Purge(ctx, "purged"))
	for _, key := range []string{"absent", "deleted", "purged"} {
		_, err := get(ctx, key)
		require.ErrorIs(t, err, jetstream.ErrKeyNotFound, key)
	}

	_, err = leaderGetOf(js, "spinifex-missing")(ctx, "live")
	require.Error(t, err)
	assert.NotErrorIs(t, err, jetstream.ErrKeyNotFound, "a missing stream is a read failure, not absence")
}
