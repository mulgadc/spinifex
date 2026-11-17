package vpcpeering

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stallingKV runs stall once, just before the first write of a key with
// prefix: a Create, or an Update when update is set.
type stallingKV struct {
	jetstream.KeyValue

	prefix string
	update bool
	stall  func()
}

func (k *stallingKV) before(key string, update bool) {
	if stall := k.stall; stall != nil && update == k.update && strings.HasPrefix(key, k.prefix) {
		k.stall = nil
		stall()
	}
}

func (k *stallingKV) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	k.before(key, false)
	return k.KeyValue.Create(ctx, key, value, opts...)
}

func (k *stallingKV) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	k.before(key, true)
	return k.KeyValue.Update(ctx, key, value, revision)
}

// stallCreate makes the next Create pause before writing a key with prefix
// while the clock passes the grace and a sweep runs.
func (f *fixture) stallCreate(t *testing.T, prefix string, check func(SweepResult)) {
	f.svc.live = &stallingKV{KeyValue: f.svc.live, prefix: prefix, stall: func() {
		f.clock.advance(claimGrace)
		res, err := f.svc.Sweep(t.Context())
		require.NoError(t, err)
		check(res)
	}}
}

func TestCreate_StalledPastSweepBeforeRecordIsFullyIndexed(t *testing.T) {
	f := newFixture(t)
	f.stallCreate(t, "pcx.", func(res SweepResult) {
		assert.Positive(t, res.KeysRemoved, "the sweep removes the stalled create's indexes")
	})
	id := *f.create(t, vpcA, vpcB).VpcPeeringConnectionId

	conns, err := f.describe(t)
	require.NoError(t, err)
	require.Len(t, conns, 1, "an unfiltered Describe finds the connection through its account index")
	assert.Equal(t, id, aws.StringValue(conns[0].VpcPeeringConnectionId))
	held, err := f.svc.vpcConnections(t.Context(), vpcB)
	require.NoError(t, err)
	require.Len(t, held, 1, "the accepter VPC's index names the connection")

	f.clock.advance(claimGrace)
	_, err = f.svc.Sweep(t.Context())
	require.NoError(t, err)
	assert.Equal(t, StatePendingAcceptance, f.status(t, id))
}

func TestCreate_RecordSweptBeforeClaimFailsTheCreate(t *testing.T) {
	f := newFixture(t)
	f.stallCreate(t, "req.", func(res SweepResult) {
		assert.Equal(t, 1, res.DiscardedStale, "the sweep discards the unclaimed record")
	})
	_, err := f.svc.CreateVpcPeeringConnection(t.Context(), &ec2.CreateVpcPeeringConnectionInput{VpcId: aws.String(vpcA), PeerVpcId: aws.String(vpcB)}, testAccount)
	requireCode(t, err, awserrors.ErrorServerInternal)

	conns, err := f.describe(t)
	require.NoError(t, err)
	assert.Empty(t, conns, "a failed create leaves no visible connection")
	id := *f.create(t, vpcA, vpcB).VpcPeeringConnectionId
	assert.Equal(t, StatePendingAcceptance, f.status(t, id), "a retried create takes the dead claim over")
}
