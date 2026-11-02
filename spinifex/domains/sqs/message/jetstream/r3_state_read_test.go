package jetstream_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	sqsjs "github.com/mulgadc/spinifex/spinifex/domains/sqs/message/jetstream"
	"github.com/nats-io/nats.go"
	jsapi "github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// TestR3GetStateAfterDeleteFromFollower reads, through stores connected
// only to the state stream's followers, records the leader has just deleted
// or purged. A follower may not have applied the change yet, so a read it
// answered itself could still return the record.
func testR3GetStateAfterDeleteFromFollower(t *testing.T) {
	const rounds, batch = 20, 200
	c := startCluster(t)
	s := c.store(t)
	ctx := context.Background()
	q0 := message.Namespace{MessageShard: 0, StateShard: 0}

	kvs, err := c.js.Stream(ctx, "KV_"+sqsjs.BucketName(q0.StateShard))
	require.NoError(t, err)
	leader := kvs.CachedInfo().Cluster.Leader
	require.NotEmpty(t, leader)

	// Opened before any delete, so no open call can stand in for a sync.
	var followers []*sqsjs.Store
	for _, srv := range c.servers {
		if srv.Name() == leader {
			continue
		}
		nc, err := nats.Connect(srv.ClientURL(), nats.NoReconnect())
		require.NoError(t, err)
		t.Cleanup(nc.Close)
		require.Equal(t, srv.Name(), nc.ConnectedServerName())
		js, err := jsapi.New(nc)
		require.NoError(t, err)
		fs, err := sqsjs.New(ctx, js, testConfig)
		require.NoError(t, err)
		followers = append(followers, fs)
	}
	require.Len(t, followers, 2)

	for r := range rounds {
		q := q0
		q.Queue = message.QueueID(fmt.Sprintf("fr-%d", r))
		first := appendTo(t, s, q, "b")
		var rev message.Revision
		for i := range batch {
			rev, err = s.PutState(ctx, q, first+message.Position(i), []byte("s"), 0)
			require.NoError(t, err)
		}
		last := first + batch - 1
		require.NoError(t, s.DeleteState(ctx, q, last, rev))
		for i, fs := range followers {
			_, err := fs.GetState(ctx, q, last)
			require.ErrorIs(t, err, message.ErrNotFound, "round %d follower %d: deleted record still read", r, i)
		}
		require.NoError(t, s.DeleteNamespace(ctx, q))
		for i, fs := range followers {
			for _, pos := range []message.Position{first, first + batch/2, last - 1} {
				_, err := fs.GetState(ctx, q, pos)
				require.ErrorIs(t, err, message.ErrNotFound, "round %d follower %d pos %d: purged record still read", r, i, pos)
			}
		}
	}
}

// TestR3StateEnumerationAfterDeleteFromFollower counts and lists state
// through stores connected only to the state stream's followers, right after
// the leader deleted it. The namespaces hold state only, so the message-body
// scan, which this does not cover, has nothing of theirs to find.
func testR3StateEnumerationAfterDeleteFromFollower(t *testing.T) {
	const rounds, batch = 20, 200
	c := startCluster(t)
	s := c.store(t)
	ctx := context.Background()
	q0 := message.Namespace{MessageShard: 1, StateShard: 1}

	kvs, err := c.js.Stream(ctx, "KV_"+sqsjs.BucketName(q0.StateShard))
	require.NoError(t, err)
	leader := kvs.CachedInfo().Cluster.Leader
	require.NotEmpty(t, leader)
	var followers []*sqsjs.Store
	for _, srv := range c.servers {
		if srv.Name() == leader {
			continue
		}
		nc, err := nats.Connect(srv.ClientURL(), nats.NoReconnect())
		require.NoError(t, err)
		t.Cleanup(nc.Close)
		require.Equal(t, srv.Name(), nc.ConnectedServerName())
		js, err := jsapi.New(nc)
		require.NoError(t, err)
		fs, err := sqsjs.New(ctx, js, testConfig)
		require.NoError(t, err)
		followers = append(followers, fs)
	}
	require.Len(t, followers, 2)

	for r := range rounds {
		q := q0
		q.Queue = message.QueueID(fmt.Sprintf("fe-%d", r))
		for i := range batch {
			_, err := s.PutState(ctx, q, message.Position(i+1), []byte("s"), 0)
			require.NoError(t, err)
		}
		require.NoError(t, s.DeleteNamespace(ctx, q))
		for i, fs := range followers {
			u, err := fs.Count(ctx, q)
			require.NoError(t, err)
			require.Zero(t, u.States, "round %d follower %d: deleted state still counted", r, i)
			got, err := fs.Namespaces(ctx, q.MessageShard)
			require.NoError(t, err)
			for _, n := range got {
				require.NotEqual(t, q.Queue, n.Namespace.Queue, "round %d follower %d: deleted namespace still listed", r, i)
			}
		}
	}
}
