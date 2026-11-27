package vpc

import (
	"context"
	"sync"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bareConflictKV lets a competitor reserve ahead of the first create, then
// answers that create's real lost race with the bare replicated-stream code.
type bareConflictKV struct {
	jetstream.KeyValue

	competitor func()
	mu         sync.Mutex
	creates    int
	updates    int
	realErr    error
}

func (k *bareConflictKV) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	k.mu.Lock()
	k.creates++
	first := k.creates == 1
	k.mu.Unlock()
	if first {
		k.competitor()
	}
	rev, err := k.KeyValue.Create(ctx, key, value, opts...)
	if !first || err == nil {
		return rev, err
	}
	k.mu.Lock()
	k.realErr = err
	k.mu.Unlock()
	// The embedded server is R1 and its answer is wrapped as ErrKeyExists. On
	// an R>1 stream, a create that loses after reading a delete marker returns
	// the server's bare 10164 reply, which this substitutes.
	return 0, &jetstream.APIError{Code: 400, ErrorCode: jetstream.JSErrCodeStreamWrongLastSequenceConstant, Description: "wrong last sequence"}
}

func (k *bareConflictKV) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	k.mu.Lock()
	k.updates++
	k.mu.Unlock()
	return k.KeyValue.Update(ctx, key, value, revision)
}

// A reservation whose create on a deleted row loses to a competitor with the
// bare replicated conflict code rereads the leader and takes the next address.
func TestIPAM_ReserveVIPRetriesBareCreateConflict(t *testing.T) {
	t.Parallel()
	js := ipamTestJS(t)
	competitor, err := NewIPAM(t.Context(), js)
	require.NoError(t, err)
	realKV, err := competitor.store.KV(t.Context())
	require.NoError(t, err)
	_, err = realKV.Put(t.Context(), vipSubnet, []byte(`{"subnet_id":"`+vipSubnet+`","cidr_block":"`+vipCIDR+`","allocated":[]}`))
	require.NoError(t, err)
	require.NoError(t, realKV.Delete(t.Context(), vipSubnet))

	conflict := &bareConflictKV{KeyValue: realKV}
	conflict.competitor = func() {
		ip, err := competitor.ReserveVIP(t.Context(), vipSubnet, vipCIDR, "lb-0b")
		assert.NoError(t, err)
		assert.Equal(t, "10.0.50.4", ip)
	}
	ipam, err := NewIPAM(t.Context(), ipamLeaderJS{JetStream: js, kv: conflict})
	require.NoError(t, err)

	ip, err := ipam.ReserveVIP(t.Context(), vipSubnet, vipCIDR, "lb-0a")
	assert.NoError(t, err, "a definite create conflict rereads the leader")
	assert.Equal(t, "10.0.50.5", ip)
	assert.ErrorIs(t, conflict.realErr, jetstream.ErrKeyExists, "the substituted create really lost to the competitor")
	assert.Equal(t, 1, conflict.creates, "the absent row is created once")
	assert.Equal(t, 1, conflict.updates, "the retry writes at the competitor's revision")
	assert.Equal(t, []IPEntry{
		{IP: "10.0.50.4", Purpose: PurposeLBVIP, OwnerID: "lb-0b"},
		{IP: "10.0.50.5", Purpose: PurposeLBVIP, OwnerID: "lb-0a"},
	}, currentIPAMEntries(t, realKV, vipSubnet))
}

var _ jetstream.KeyValue = (*bareConflictKV)(nil)
