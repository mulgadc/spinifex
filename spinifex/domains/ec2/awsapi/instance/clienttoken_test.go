package instance

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/lifecycle/idempotency"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const ctTestAccount = "111122223333"

// RunInstances records predate per-action namespacing: they live at the bare
// account/token key, hashed over the request minus its token. A record written
// that way must still replay, or a retry across an upgrade would re-launch.
func TestRunInstances_ClientTokenReplaysUnnamespacedRecord(t *testing.T) {
	t.Parallel()
	_, nc, _ := testutil.StartTestJetStream(t)
	kv, err := idempotency.OpenBucket(t.Context(), testutil.NewJetStream(t, nc), "ec2-tokens", time.Minute)
	require.NoError(t, err)
	bucket := func(context.Context) (jetstream.KeyValue, error) { return kv, nil }

	const tok = "rt-1"
	input := defaults
	input.ClientToken = aws.String(tok)
	unTokened := input
	unTokened.ClientToken = nil
	hash := idempotency.ParamHash(&unTokened)

	store := idempotency.NewStore[ec2.Reservation](kv, "")
	_, owned, err := store.Claim(t.Context(), ctTestAccount, tok, hash)
	require.NoError(t, err)
	require.True(t, owned)
	require.NoError(t, store.Finalize(t.Context(), ctTestAccount, tok, hash, ec2.Reservation{ReservationId: aws.String("r-x")}))
	_, err = kv.Get(t.Context(), idempotency.Key(ctTestAccount, tok))
	require.NoError(t, err, "record must sit at the unprefixed key")

	// A nil NATS connection: replay must not reach the launch path.
	replay := input
	res, err := RunInstances(t.Context(), &replay, nil, bucket, nil, ctTestAccount, nil, nil, 1)
	require.NoError(t, err)
	assert.Equal(t, "r-x", aws.StringValue(res.ReservationId))

	changed := input
	changed.InstanceType = aws.String("t3.large")
	_, err = RunInstances(t.Context(), &changed, nil, bucket, nil, ctTestAccount, nil, nil, 1)
	require.EqualError(t, err, awserrors.ErrorIdempotentParameterMismatch)
}
