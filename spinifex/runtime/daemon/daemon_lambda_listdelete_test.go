package daemon

import (
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	lambdafunction "github.com/mulgadc/spinifex/spinifex/domains/lambda/function"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A daemon without the list and delete subjects refuses both as retryable
// transport errors and changes nothing; once served, they decode and commit.
func TestLambdaListDelete_PreviousDaemonRefusesWithoutMutation(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	svc := lambdafunction.NewMetaService(js)
	client := lambdafunction.NewNATSClient(nc)
	ctx := t.Context()
	const region = "ap-southeast-2"

	serveLambdaSubjects(t, nc, svc, true)
	_, err := client.Create(ctx, lambdaTestAccount, region, lambdafunction.Record{
		FunctionID: "f1", FunctionName: "hello", Role: "arn:aws:iam::000000000001:role/r", Runtime: "provided.al2023",
		Handler: "bootstrap", Timeout: 3, MemorySize: 128, Code: lambdafunction.Code{RevisionID: "rev-1"},
	})
	require.NoError(t, err)
	rec, rev, err := client.Get(ctx, lambdaTestAccount, region, "hello")
	require.NoError(t, err)

	_, _, err = client.List(ctx, lambdaTestAccount, region, "", 1)
	require.ErrorIs(t, err, nats.ErrNoResponders)
	err = client.Delete(ctx, lambdaTestAccount, region, rec, rev)
	require.ErrorIs(t, err, nats.ErrNoResponders)
	_, coded := awserrors.ResolveErrorCode(err)
	assert.False(t, coded, "the gateway maps an uncoded failure to a retryable ServiceException")
	same, sameRev, err := client.Get(ctx, lambdaTestAccount, region, "hello")
	require.NoError(t, err)
	assert.Equal(t, rev, sameRev)
	assert.Equal(t, rec, same)
	kv, err := js.KeyValue(ctx, lambdafunction.KVAccountBucket(lambdaTestAccount))
	require.NoError(t, err)
	_, err = kv.Get(ctx, lambdafunction.KVDeletedKey(region, "f1"))
	assert.ErrorIs(t, err, jetstream.ErrKeyNotFound, "no obligation was recorded")

	for subject, h := range map[string]nats.MsgHandler{
		lambdafunction.SubjectList:   asMsgHandler(handleNATSRequest("node-1", svc.List)),
		lambdafunction.SubjectDelete: asMsgHandler(handleNATSRequest("node-1", svc.Delete)),
	} {
		sub, err := nc.QueueSubscribe(subject, "spinifex-workers", h)
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}
	require.NoError(t, nc.Flush())
	listed, next, err := client.List(ctx, lambdaTestAccount, region, "", 1)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, rec, listed[0].Record)
	assert.Equal(t, rev, listed[0].Revision)
	assert.Empty(t, next)

	_, _, err = client.List(ctx, lambdaTestAccount, region, "bogus", 1)
	testutil.RequireAWSCode(t, err, awserrors.ErrorInvalidParameterValueException)

	require.NoError(t, client.Delete(ctx, lambdaTestAccount, region, rec, rev))
	_, _, err = client.Get(ctx, lambdaTestAccount, region, "hello")
	require.ErrorIs(t, err, lambdafunction.ErrNotFound)
	err = client.Delete(ctx, lambdaTestAccount, region, rec, rev)
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceNotFoundException)
}
