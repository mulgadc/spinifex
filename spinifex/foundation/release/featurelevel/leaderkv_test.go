package featurelevel

import (
	"strings"
	"sync"
	"testing"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Reads of the record go to the stream leader (a message get), never a direct
// get that a lagging follower may answer.
func TestLeaderKV_ReadsThroughTheLeaderNotADirectGet(t *testing.T) {
	t.Parallel()
	_, nc, js := testutil.StartTestJetStream(t)
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: "cluster-config-leader", History: 1})
	require.NoError(t, err)
	s := NewStore(LeaderKV(js, kv))

	var mu sync.Mutex
	var subjects []string
	sub, err := nc.Subscribe("$JS.API.>", func(m *nats.Msg) {
		mu.Lock()
		subjects = append(subjects, m.Subject)
		mu.Unlock()
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	rec, rev, err := s.Read(t.Context())
	require.NoError(t, err)
	assert.Zero(t, rev)
	assert.Zero(t, rec.Level, "an absent key reads as level 0")

	_, adv, err := s.Advance(t.Context(), members(1, true))
	require.NoError(t, err)
	require.True(t, adv)
	rec, rev, err = s.Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, rec.Level)
	assert.NotZero(t, rev)

	require.NoError(t, nc.Flush())
	mu.Lock()
	defer mu.Unlock()
	gets := 0
	for _, subj := range subjects {
		assert.False(t, strings.HasPrefix(subj, "$JS.API.DIRECT.GET."), "feature level read used a direct get: %s", subj)
		if subj == "$JS.API.STREAM.MSG.GET.KV_cluster-config-leader" {
			gets++
		}
	}
	assert.GreaterOrEqual(t, gets, 3, "every Read is a leader message get")
}

func TestLeaderKV_ADeletedKeyReadsAsAbsent(t *testing.T) {
	t.Parallel()
	_, _, js := testutil.StartTestJetStream(t)
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: "cluster-config-del", History: 1})
	require.NoError(t, err)
	s := NewStore(LeaderKV(js, kv))
	_, _, err = s.Advance(t.Context(), members(1, true))
	require.NoError(t, err)
	require.NoError(t, kv.Delete(t.Context(), Key))
	rec, rev, err := s.Read(t.Context())
	require.NoError(t, err)
	assert.Zero(t, rec.Level)
	assert.Zero(t, rev)
}
