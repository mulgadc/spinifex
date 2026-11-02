package jetstream_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	sqsjs "github.com/mulgadc/spinifex/spinifex/domains/sqs/message/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stateBucketRequests splits recorded API subjects for one state bucket's
// stream into leader message gets and anything else.
func stateBucketRequests(subjects []string, bucket string) (leaderGets int, other []string) {
	for _, subj := range subjects {
		if !strings.Contains(subj, ".KV_"+bucket) {
			continue
		}
		if subj == "$JS.API.STREAM.MSG.GET.KV_"+bucket {
			leaderGets++
			continue
		}
		other = append(other, subj)
	}
	return leaderGets, other
}

// TestStateEnumerationIsLeaderServed pins the route Count and Namespaces use
// for state keys: one leader message get per record under the prefix, then
// one that finds none, with no direct get and no consumer on the bucket.
func TestStateEnumerationIsLeaderServed(t *testing.T) {
	srv, js := startServer(t, t.TempDir())
	s, err := sqsjs.New(context.Background(), js, testConfig)
	require.NoError(t, err)
	ctx := context.Background()
	q := ns(0, 1, "enum")
	other := ns(0, 1, "enum-other")
	pos := appendTo(t, s, q, "b")
	for i := range 3 {
		_, err := s.PutState(ctx, q, pos+message.Position(i), []byte("v"), 0)
		require.NoError(t, err)
	}
	rev, err := s.PutState(ctx, q, pos+3, []byte("v"), 0)
	require.NoError(t, err)
	require.NoError(t, s.DeleteState(ctx, q, pos+3, rev))
	// A later record outside the prefix sets the bound past the last match.
	_, err = s.PutState(ctx, other, pos, []byte("v"), 0)
	require.NoError(t, err)
	bucket := sqsjs.BucketName(1)

	rec := recordAPI(t, srv.ClientURL())
	rec.drain(t, js.Conn())
	u, err := s.Count(ctx, q)
	require.NoError(t, err)
	assert.Equal(t, uint64(3), u.States)
	gets, rest := stateBucketRequests(rec.drain(t, js.Conn()), bucket)
	// Three puts and one delete marker, then the get that finds nothing more.
	assert.Equal(t, 5, gets)
	for _, subj := range rest {
		assert.True(t, strings.HasPrefix(subj, "$JS.API.STREAM.INFO."), "Count sent %s for the state bucket", subj)
	}

	got, err := s.Namespaces(ctx, 0)
	require.NoError(t, err)
	states := map[message.QueueID]uint64{}
	for _, n := range got {
		states[n.Namespace.Queue] = n.Usage.States
	}
	assert.Equal(t, map[message.QueueID]uint64{q.Queue: 3, other.Queue: 1}, states)
	for _, subj := range rec.drain(t, js.Conn()) {
		if strings.Contains(subj, ".KV_") {
			assert.False(t, strings.HasPrefix(subj, "$JS.API.DIRECT.GET."), "Namespaces used a direct get: %s", subj)
			assert.NotContains(t, subj, "CONSUMER.", "Namespaces created a consumer on a state bucket: %s", subj)
		}
	}
}

// TestStateEnumerationSkipsGapsAndOtherNamespaces counts through removed
// revisions and interleaved records of neighbouring namespaces.
func TestStateEnumerationSkipsGapsAndOtherNamespaces(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	q := ns(1, 0, "gap")
	near := ns(1, 0, "gap-near")
	far := ns(2, 0, "gap")
	pos := appendTo(t, s, q, "b")
	revs := map[message.Position]message.Revision{}
	for round := range 4 {
		for i := range 5 {
			p := pos + message.Position(i)
			// History 1 drops each earlier revision, leaving gaps.
			r, err := s.PutState(ctx, q, p, []byte{byte(round)}, revs[p])
			require.NoError(t, err)
			revs[p] = r
			_, err = s.PutState(ctx, near, p+message.Position(100*round), []byte("n"), 0)
			require.NoError(t, err)
			_, err = s.PutState(ctx, far, p+message.Position(100*round), []byte("f"), 0)
			require.NoError(t, err)
		}
	}
	requireUsage(t, s, q, message.Usage{Messages: 1, States: 5})
	requireUsage(t, s, near, message.Usage{States: 20})
	require.NoError(t, s.DeleteState(ctx, q, pos, revs[pos]))
	requireUsage(t, s, q, message.Usage{Messages: 1, States: 4})
}

// TestStateEnumerationMarkers drops a key at its delete or purge marker and
// counts it again when a later write follows.
func TestStateEnumerationMarkers(t *testing.T) {
	s, js := newStore(t)
	ctx := context.Background()
	q := ns(0, 0, "mark")
	pos := appendTo(t, s, q, "b")
	kv, err := js.KeyValue(ctx, sqsjs.BucketName(0))
	require.NoError(t, err)

	for i := range 3 {
		_, err := s.PutState(ctx, q, pos+message.Position(i), []byte("v"), 0)
		require.NoError(t, err)
	}
	require.NoError(t, kv.Delete(ctx, sqsjs.StateKey(q, pos)))
	require.NoError(t, kv.Purge(ctx, sqsjs.StateKey(q, pos+1)))
	requireUsage(t, s, q, message.Usage{Messages: 1, States: 1})

	_, err = s.PutState(ctx, q, pos, []byte("again"), 0)
	require.NoError(t, err)
	requireUsage(t, s, q, message.Usage{Messages: 1, States: 2})
}

func TestStateEnumerationCancelled(t *testing.T) {
	s, _ := newStore(t)
	q := ns(0, 0, "cancel")
	pos := appendTo(t, s, q, "b")
	_, err := s.PutState(context.Background(), q, pos, []byte("v"), 0)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = s.Count(ctx, q)
	require.ErrorIs(t, err, context.Canceled)
	_, err = s.Namespaces(ctx, 0)
	require.ErrorIs(t, err, context.Canceled)
}
