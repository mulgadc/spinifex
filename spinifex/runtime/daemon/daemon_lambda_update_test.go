package daemon

import (
	"context"
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	lambdafunction "github.com/mulgadc/spinifex/spinifex/domains/lambda/function"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const lambdaTestAccount = "000000000001"

func serveLambdaSubjects(t *testing.T, nc *nats.Conn, svc *lambdafunction.MetaService, withUpdate bool) {
	t.Helper()
	handlers := map[string]nats.MsgHandler{
		lambdafunction.SubjectCreate:         asMsgHandler(handleNATSRequest("node-1", svc.Create)),
		lambdafunction.SubjectGet:            asMsgHandler(handleNATSRequest("node-1", svc.Get)),
		lambdafunction.SubjectCodeStored:     asMsgHandler(handleNATSRequest("node-1", svc.CodeStored)),
		lambdafunction.SubjectRollbackCreate: asMsgHandler(handleNATSRequest("node-1", svc.RollbackCreate)),
	}
	if withUpdate {
		handlers[lambdafunction.SubjectCodeUpdateBegin] = asMsgHandler(handleNATSRequest("node-1", svc.BeginCodeUpdate))
		handlers[lambdafunction.SubjectCodeUpdated] = asMsgHandler(handleNATSRequest("node-1", svc.CodeUpdated))
	}
	for subject, h := range handlers {
		sub, err := nc.QueueSubscribe(subject, "spinifex-workers", h)
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}
	require.NoError(t, nc.Flush())
}

// A gateway that can update code may reach a daemon that predates it: the
// request must fail as a retryable transport error and change nothing, and the
// existing Get path must read a record an upgraded daemon has updated.
func TestLambdaCodeUpdate_PreviousDaemonRefusesWithoutMutation(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	svc := lambdafunction.NewMetaService(js)
	svc.SetReadiness(func(context.Context, lambdafunction.Record) error { return nil })
	client := lambdafunction.NewNATSClient(nc)
	ctx := t.Context()
	const region, digest = "ap-southeast-2", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	serveLambdaSubjects(t, nc, svc, false)
	rev, err := client.Create(ctx, lambdaTestAccount, region, lambdafunction.Record{
		FunctionID: "f1", FunctionName: "hello", Role: "arn:aws:iam::000000000001:role/r", Runtime: "provided.al2023",
		Handler: "bootstrap", Timeout: 3, MemorySize: 128, Code: lambdafunction.Code{RevisionID: "rev-1"},
	})
	require.NoError(t, err)
	_, _, err = client.CodeStored(ctx, lambdaTestAccount, region, "hello", rev, digest, 9)
	require.NoError(t, err)
	require.NoError(t, svc.ActivatePending(ctx))
	rec, rev, err := client.Get(ctx, lambdaTestAccount, region, "hello")
	require.NoError(t, err)
	require.Equal(t, lambdafunction.StateActive, rec.State)

	err = client.BeginCodeUpdate(ctx, lambdaTestAccount, region, rec, rev, "rev-2")
	require.ErrorIs(t, err, nats.ErrNoResponders)
	_, coded := awserrors.ResolveErrorCode(err)
	assert.False(t, coded, "the gateway maps an uncoded failure to a retryable ServiceException")
	_, _, err = client.CodeUpdated(ctx, lambdaTestAccount, region, rec, rev, "rev-2", digest, 9)
	require.ErrorIs(t, err, nats.ErrNoResponders)
	same, sameRev, err := client.Get(ctx, lambdaTestAccount, region, "hello")
	require.NoError(t, err)
	assert.Equal(t, rev, sameRev)
	assert.Equal(t, rec, same)
	kv, err := js.KeyValue(ctx, lambdafunction.KVAccountBucket(lambdaTestAccount))
	require.NoError(t, err)
	_, err = kv.Get(ctx, lambdafunction.KVUploadKey(region, "f1", "rev-2"))
	assert.ErrorIs(t, err, jetstream.ErrKeyNotFound, "no upload was recorded")

	// Once the daemon serves the new subjects the update lands, and the old
	// Get request still decodes the updated record.
	serveLambdaSubjects(t, nc, svc, true)
	const newDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	require.NoError(t, client.BeginCodeUpdate(ctx, lambdaTestAccount, region, rec, rev, "rev-2"))
	_, updatedRev, err := client.CodeUpdated(ctx, lambdaTestAccount, region, rec, rev, "rev-2", newDigest, 12)
	require.NoError(t, err)
	got, gotRev, err := client.Get(ctx, lambdaTestAccount, region, "hello")
	require.NoError(t, err)
	assert.Equal(t, updatedRev, gotRev)
	assert.Equal(t, lambdafunction.FormatVersion, got.FormatVersion)
	assert.Equal(t, lambdafunction.Code{RevisionID: "rev-2", SHA256: newDigest, Size: 12, State: lambdafunction.CodeStored}, got.Code)
	assert.Equal(t, lambdafunction.StateActive, got.State)
}
