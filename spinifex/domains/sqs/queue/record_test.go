package queue

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An N-1 reader must write back fields an N writer added, byte for byte,
// including inside the attribute set (ADR-0006:S4).
func TestRecordPreservesUnknownFields(t *testing.T) {
	in := `{"v":2,"queueId":"Q1","messageShard":3,"stateShard":5,"account":"111122223333",` +
		`"region":"ap-southeast-2","name":"orders","arn":"arn:aws:sqs:ap-southeast-2:111122223333:orders",` +
		`"createdAt":"2026-10-30T00:00:00Z","type":"standard","state":"active","generation":4,` +
		`"attributes":{"DelaySeconds":"5","FutureAttribute":"on"},"attributesModifiedAt":"2026-10-30T00:00:01Z",` +
		`"fifoThroughputLimit":null,"futureBlock":{"a":[1,2,{"b":"c"}]}}`

	r, err := DecodeRecord([]byte(in))
	require.NoError(t, err)
	assert.Equal(t, message.Shard(3), r.MessageShard)
	assert.Equal(t, "on", r.Attributes["FutureAttribute"])

	r.Generation++
	out, err := r.Encode()
	require.NoError(t, err)

	var got map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out, &got))
	assert.JSONEq(t, `{"a":[1,2,{"b":"c"}]}`, string(got["futureBlock"]))
	assert.Equal(t, "null", string(got["fifoThroughputLimit"]))
	assert.Equal(t, "5", string(got["generation"]))
	assert.Equal(t, "2", string(got["v"]))
}

func TestNewQueueIDIsStoreSafeAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		id, err := NewQueueID()
		require.NoError(t, err)
		assert.Len(t, id, 26)
		require.NoError(t, message.Namespace{Queue: message.QueueID(id)}.Validate())
		assert.False(t, seen[id], "duplicate queue ID %s", id)
		seen[id] = true
	}
}

func TestAllocateIsStableAndInRange(t *testing.T) {
	counts := map[message.Shard]int{}
	for range 2000 {
		id, err := NewQueueID()
		require.NoError(t, err)
		m, s, err := Allocate(id, 16, 8)
		require.NoError(t, err)
		require.Less(t, uint32(m), uint32(16))
		require.Less(t, uint32(s), uint32(8))
		m2, s2, _ := Allocate(id, 16, 8)
		assert.Equal(t, m, m2)
		assert.Equal(t, s, s2)
		counts[m]++
	}
	assert.Len(t, counts, 16, "every message shard should receive queues")

	_, _, err := Allocate("x", 0, 8)
	assert.Error(t, err)
	_, _, err = Allocate("x", 8, 0)
	assert.Error(t, err)
}

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"a", "Orders_2026-q1", strings.Repeat("x", 80)} {
		assert.NoError(t, ValidateName(ok), ok)
	}
	for _, bad := range []string{"", strings.Repeat("x", 81), "orders.fifo", "has space", "slash/name", "ünicode"} {
		assert.ErrorIs(t, ValidateName(bad), ErrInvalidQueueName, bad)
	}
}
