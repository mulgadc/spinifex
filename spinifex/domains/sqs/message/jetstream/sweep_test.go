package jetstream_test

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

type swept struct {
	ns  message.Namespace
	pos message.Position
}

func withState(t *testing.T, s message.Store, ns message.Namespace, value string) swept {
	t.Helper()
	res, err := s.Append(context.Background(), ns, message.AppendInput{Body: []byte("m")})
	require.NoError(t, err)
	if value != "" {
		_, err := s.PutState(context.Background(), ns, res.Position, []byte(value), 0)
		require.NoError(t, err)
	}
	return swept{ns, res.Position}
}

func requireBody(t *testing.T, s message.Store, m swept, want bool) {
	t.Helper()
	_, err := s.Get(context.Background(), m.ns, m.pos)
	if want {
		require.NoError(t, err, "%+v body kept", m)
	} else {
		require.ErrorIs(t, err, message.ErrNotFound, "%+v body swept", m)
	}
}

func requireState(t *testing.T, s message.Store, m swept, want bool) {
	t.Helper()
	_, err := s.GetState(context.Background(), m.ns, m.pos)
	if want {
		require.NoError(t, err, "%+v state kept", m)
	} else {
		require.ErrorIs(t, err, message.ErrNotFound, "%+v state swept", m)
	}
}

func TestSweepDeletesOnlyTombstonedBodies(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	q, neighbour, other := ns(1, 1, "q"), ns(1, 0, "n"), ns(2, 1, "q")
	noState := withState(t, s, q, "")
	live := withState(t, s, q, "inflight")
	var dead []swept
	for _, v := range []string{"deleted", "expired", "moved"} {
		dead = append(dead, withState(t, s, q, v))
	}
	dead = append(dead, withState(t, s, neighbour, "deleted"))
	otherShard := withState(t, s, other, "deleted")

	res, err := message.Sweep(ctx, s, 1, terminal, 2)
	require.NoError(t, err)
	assert.Equal(t, message.SweepResult{Scanned: 6, Swept: 4}, res)
	requireBody(t, s, noState, true)
	requireBody(t, s, live, true)
	requireState(t, s, live, true)
	for _, m := range dead {
		requireBody(t, s, m, false)
		requireState(t, s, m, false)
	}
	requireBody(t, s, otherShard, true)
	requireState(t, s, otherShard, true)

	res, err = message.Sweep(ctx, s, 1, terminal, 2)
	require.NoError(t, err)
	assert.Equal(t, message.SweepResult{Scanned: 2}, res, "a rerun finds nothing more")
}

// failStateDelete crashes the sweeper after it deletes a body and before it
// deletes the body's state record.
type failStateDelete struct{ message.Store }

var errCrash = errors.New("crash")

func (failStateDelete) DeleteState(context.Context, message.Namespace, message.Position, message.Revision) error {
	return errCrash
}

func TestSweepCrashBetweenBodyAndStateDelete(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	m := withState(t, s, ns(0, 1, "q"), "deleted")

	_, err := message.Sweep(ctx, failStateDelete{s}, 0, terminal, 10)
	require.ErrorIs(t, err, errCrash)
	requireBody(t, s, m, false)
	requireState(t, s, m, true)

	res, err := message.Sweep(ctx, s, 0, terminal, 10)
	require.NoError(t, err, "the retry runs clean")
	assert.Equal(t, message.SweepResult{}, res, "the body never resurfaces to be swept again")
	requireBody(t, s, m, false)
	// The bodiless record reads as gone and is left to the state bucket's
	// retention backstop: a pass walks bodies, so it cannot find it.
	requireState(t, s, m, true)
	requireUsage(t, s, m.ns, message.Usage{States: 1})
}
