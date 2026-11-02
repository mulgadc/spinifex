package conformance_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// terminal stands in for the domain's decoding of its state records.
func terminal(st message.State) bool {
	switch string(st.Value) {
	case "deleted", "expired", "moved":
		return true
	}
	return false
}

func put(t *testing.T, s message.Store, ns message.Namespace, value string) message.Position {
	t.Helper()
	ctx := context.Background()
	res, err := s.Append(ctx, ns, message.AppendInput{Body: []byte("m")})
	require.NoError(t, err)
	if value != "" {
		_, err := s.PutState(ctx, ns, res.Position, []byte(value), 0)
		require.NoError(t, err)
	}
	return res.Position
}

func exists(s message.Store, ns message.Namespace, pos message.Position) (body, state bool) {
	ctx := context.Background()
	_, err := s.Get(ctx, ns, pos)
	body = err == nil
	_, err = s.GetState(ctx, ns, pos)
	return body, err == nil
}

func TestSweepOnReferenceFake(t *testing.T) {
	s := &memStore{d: newMemData()}
	ctx := context.Background()
	q, other := message.Namespace{Queue: "q", StateShard: 1}, message.Namespace{MessageShard: 1, Queue: "q"}
	noState, live := put(t, s, q, ""), put(t, s, q, "inflight")
	dead := []message.Position{put(t, s, q, "deleted"), put(t, s, q, "expired"), put(t, s, q, "moved")}
	otherShard := put(t, s, other, "deleted")

	res, err := message.Sweep(ctx, s, 0, terminal, 2)
	require.NoError(t, err)
	assert.Equal(t, message.SweepResult{Scanned: 5, Swept: 3}, res)
	for _, p := range []message.Position{noState, live} {
		body, _ := exists(s, q, p)
		assert.True(t, body, "position %d kept", p)
	}
	_, liveState := exists(s, q, live)
	assert.True(t, liveState, "a live record is kept")
	for _, p := range dead {
		body, state := exists(s, q, p)
		assert.False(t, body || state, "tombstoned position %d swept", p)
	}
	body, state := exists(s, other, otherShard)
	assert.True(t, body && state, "another shard is untouched")

	res, err = message.Sweep(ctx, s, 0, terminal, 2)
	require.NoError(t, err)
	assert.Equal(t, message.SweepResult{Scanned: 2}, res, "a rerun finds nothing more")
}

// failStateDelete crashes the sweeper after its body delete succeeds and
// before its state delete.
type failStateDelete struct{ message.Store }

var errCrash = errors.New("crash")

func (failStateDelete) DeleteState(context.Context, message.Namespace, message.Position, message.Revision) error {
	return errCrash
}

func TestSweepRetryAfterStateDeleteFailsOnReferenceFake(t *testing.T) {
	s := &memStore{d: newMemData()}
	ctx := context.Background()
	q := message.Namespace{Queue: "q"}
	p := put(t, s, q, "deleted")

	_, err := message.Sweep(ctx, failStateDelete{s}, 0, terminal, 10)
	require.ErrorIs(t, err, errCrash)
	body, state := exists(s, q, p)
	require.False(t, body, "the body delete succeeded")
	require.True(t, state, "the state delete did not")

	res, err := message.Sweep(ctx, s, 0, terminal, 10)
	require.NoError(t, err)
	assert.Equal(t, message.SweepResult{}, res, "the body never resurfaces to be swept again")
	body, state = exists(s, q, p)
	assert.False(t, body)
	assert.True(t, state, "the bodiless record reads as gone and is left to the backstop")
}
