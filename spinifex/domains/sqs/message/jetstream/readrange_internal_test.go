package jetstream

import (
	"context"
	"testing"

	jsapi "github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rangeStream creates stream "r" holding five messages on "r.a" at
// positions 1 to 5, with position 3 removed.
func rangeStream(t *testing.T) jsapi.JetStream {
	t.Helper()
	js := startJetStream(t)
	ctx := context.Background()
	st, err := js.CreateStream(ctx, jsapi.StreamConfig{Name: "r", Subjects: []string{"r.>"}})
	require.NoError(t, err)
	for range 5 {
		_, err := js.Publish(ctx, "r.a", []byte("b"))
		require.NoError(t, err)
	}
	require.NoError(t, st.DeleteMsg(ctx, 3))
	return js
}

func collect(seqs *[]uint64, stopAt uint64) func(*jsapi.RawStreamMsg) (bool, error) {
	return func(m *jsapi.RawStreamMsg) (bool, error) {
		*seqs = append(*seqs, m.Sequence)
		return m.Sequence != stopAt, nil
	}
}

// TestReadRangeStopsAtTheBound never passes fn a match beyond the bound,
// even when the bound itself is a removed position.
func TestReadRangeStopsAtTheBound(t *testing.T) {
	js := rangeStream(t)
	var seqs []uint64
	last, done, err := readRange(context.Background(), js, "r", "r.>", 0, 3, collect(&seqs, 0))
	require.NoError(t, err)
	assert.Equal(t, []uint64{1, 2}, seqs)
	assert.Equal(t, uint64(3), last)
	assert.True(t, done)
}

// TestReadRangeDoneWhenStoppedAtTheBound reports the pass complete when fn
// stops on the bound's own match, and incomplete when it stops earlier.
func TestReadRangeDoneWhenStoppedAtTheBound(t *testing.T) {
	js := rangeStream(t)
	var seqs []uint64
	last, done, err := readRange(context.Background(), js, "r", "r.>", 0, 4, collect(&seqs, 4))
	require.NoError(t, err)
	assert.Equal(t, []uint64{1, 2, 4}, seqs)
	assert.Equal(t, uint64(4), last)
	assert.True(t, done)

	seqs = nil
	last, done, err = readRange(context.Background(), js, "r", "r.>", 0, 5, collect(&seqs, 2))
	require.NoError(t, err)
	assert.Equal(t, []uint64{1, 2}, seqs)
	assert.Equal(t, uint64(2), last)
	assert.False(t, done)
}

// TestReadRangeReturnsRequestErrors surfaces a failed leader read rather
// than reporting the pass complete.
func TestReadRangeReturnsRequestErrors(t *testing.T) {
	js := rangeStream(t)
	var seqs []uint64
	_, _, err := readRange(context.Background(), js, "r", "r.>", 0, 1, collect(&seqs, 0))
	require.NoError(t, err)
	js.Conn().Close()
	last, done, err := readRange(context.Background(), js, "r", "r.>", 1, 5, collect(&seqs, 0))
	require.Error(t, err)
	assert.Equal(t, uint64(1), last)
	assert.False(t, done)
}
