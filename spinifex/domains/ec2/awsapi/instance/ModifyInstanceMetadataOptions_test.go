package instance

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/contracts/ec2/v1"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A nil, empty, or non-"i-" instance ID is rejected at the gateway before any
// NATS round-trip, so a nil connection is never dereferenced.
func TestModifyInstanceMetadataOptions_RejectsMalformedID(t *testing.T) {
	t.Parallel()
	cases := map[string]*ec2.ModifyInstanceMetadataOptionsInput{
		"nil input":  nil,
		"nil id":     {},
		"empty id":   {InstanceId: aws.String("")},
		"bad prefix": {InstanceId: aws.String("x-12345")},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := ModifyInstanceMetadataOptions(context.Background(), in, nil, "acct")
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorInvalidInstanceIDMalformed, err.Error())
		})
	}
}

const metaTestID = "i-0123456789abcdef0"

func metaInput(hop int64) *ec2.ModifyInstanceMetadataOptionsInput {
	return &ec2.ModifyInstanceMetadataOptionsInput{
		InstanceId:              aws.String(metaTestID),
		HttpPutResponseHopLimit: aws.Int64(hop),
	}
}

// ownerReply answers one ec2.cmd request, optionally carrying the handled marker.
func ownerReply(t *testing.T, nc *nats.Conn, handled bool, body []byte, calls *atomic.Int32) {
	t.Helper()
	_, err := nc.Subscribe(ec2v1.InstanceCommandSubject(metaTestID), func(msg *nats.Msg) {
		calls.Add(1)
		reply := nats.NewMsg(msg.Reply)
		reply.Data = body
		if handled {
			reply.Header.Set(ec2v1.HandledHeader, "1")
		}
		_ = msg.RespondMsg(reply)
	})
	require.NoError(t, err)
}

func queueReply(t *testing.T, nc *nats.Conn, body []byte, calls *atomic.Int32) {
	t.Helper()
	_, err := nc.QueueSubscribe("ec2.ModifyInstanceMetadataOptions", "spinifex-workers", func(msg *nats.Msg) {
		calls.Add(1)
		_ = msg.Respond(body)
	})
	require.NoError(t, err)
}

func metaOutputJSON(t *testing.T, hop int64) []byte {
	t.Helper()
	b, err := json.Marshal(&ec2.ModifyInstanceMetadataOptionsOutput{
		InstanceId:              aws.String(metaTestID),
		InstanceMetadataOptions: &ec2.InstanceMetadataOptionsResponse{HttpPutResponseHopLimit: aws.Int64(hop)},
	})
	require.NoError(t, err)
	return b
}

func TestModifyInstanceMetadataOptions_OwnerAnswerReturnedAsIs(t *testing.T) {
	_, nc := startTestNATSServer(t)
	var owner, queue atomic.Int32
	ownerReply(t, nc, true, metaOutputJSON(t, 3), &owner)
	queueReply(t, nc, []byte(`{}`), &queue)

	out, err := ModifyInstanceMetadataOptions(context.Background(), metaInput(3), nc, "acct")
	require.NoError(t, err)
	assert.Equal(t, int64(3), aws.Int64Value(out.InstanceMetadataOptions.HttpPutResponseHopLimit))
	assert.Equal(t, int32(1), owner.Load())
	assert.Zero(t, queue.Load())
}

func TestModifyInstanceMetadataOptions_OwnerErrorWithHeaderIsNotRetried(t *testing.T) {
	_, nc := startTestNATSServer(t)
	var owner, queue atomic.Int32
	ownerReply(t, nc, true, awserrors.GenerateErrorPayload(awserrors.ErrorServerInternal), &owner)
	queueReply(t, nc, metaOutputJSON(t, 3), &queue)

	_, err := ModifyInstanceMetadataOptions(context.Background(), metaInput(3), nc, "acct")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
	assert.Zero(t, queue.Load())
}

func TestModifyInstanceMetadataOptions_OwnerValidationErrorRelayed(t *testing.T) {
	_, nc := startTestNATSServer(t)
	var owner, queue atomic.Int32
	ownerReply(t, nc, true, awserrors.GenerateErrorPayload(awserrors.ErrorUnsupportedOperation), &owner)
	queueReply(t, nc, metaOutputJSON(t, 3), &queue)

	_, err := ModifyInstanceMetadataOptions(context.Background(), metaInput(3), nc, "acct")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorUnsupportedOperation, err.Error())
	assert.Zero(t, queue.Load())
}

func TestModifyInstanceMetadataOptions_HeaderlessOtherErrorReturnedAsIs(t *testing.T) {
	_, nc := startTestNATSServer(t)
	var owner, queue atomic.Int32
	ownerReply(t, nc, false, awserrors.GenerateErrorPayload(awserrors.ErrorInvalidInstanceIDNotFound), &owner)
	queueReply(t, nc, metaOutputJSON(t, 3), &queue)

	_, err := ModifyInstanceMetadataOptions(context.Background(), metaInput(3), nc, "acct")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
	assert.Zero(t, queue.Load())
}

func TestS9_N1OwnerRejectionFallsBackOnceToQueueGroup(t *testing.T) {
	_, nc := startTestNATSServer(t)
	var owner, queue atomic.Int32
	ownerReply(t, nc, false, awserrors.GenerateErrorPayload(awserrors.ErrorServerInternal), &owner)
	queueReply(t, nc, metaOutputJSON(t, 3), &queue)

	out, err := ModifyInstanceMetadataOptions(context.Background(), metaInput(3), nc, "acct")
	require.NoError(t, err)
	assert.Equal(t, int64(3), aws.Int64Value(out.InstanceMetadataOptions.HttpPutResponseHopLimit))
	assert.Equal(t, int32(1), owner.Load(), "exactly one owner request")
	assert.Equal(t, int32(1), queue.Load(), "exactly one queue-group request")
}

func TestModifyInstanceMetadataOptions_NoOwnerUsesQueueGroupAfterThreeAttempts(t *testing.T) {
	_, nc := startTestNATSServer(t)
	var sleeps atomic.Int32
	prev := cmdRetrySleep
	cmdRetrySleep = func(time.Duration) { sleeps.Add(1) }
	t.Cleanup(func() { cmdRetrySleep = prev })

	var queue atomic.Int32
	queueReply(t, nc, metaOutputJSON(t, 2), &queue)

	out, err := ModifyInstanceMetadataOptions(context.Background(), metaInput(2), nc, "acct")
	require.NoError(t, err)
	assert.Equal(t, int64(2), aws.Int64Value(out.InstanceMetadataOptions.HttpPutResponseHopLimit))
	assert.Equal(t, int32(2), sleeps.Load(), "three attempts means two backoffs")
	assert.Equal(t, int32(1), queue.Load())
}

func TestModifyInstanceMetadataOptions_AbsentIsNotFound(t *testing.T) {
	_, nc := startTestNATSServer(t)
	var queue atomic.Int32
	queueReply(t, nc, awserrors.GenerateErrorPayload(awserrors.ErrorInvalidInstanceIDNotFound), &queue)

	_, err := ModifyInstanceMetadataOptions(context.Background(), metaInput(2), nc, "acct")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
}

func TestModifyInstanceMetadataOptions_OwnerTimeoutIsServerInternalWithoutFallback(t *testing.T) {
	_, nc := startTestNATSServer(t)
	prev := instanceCmdTimeout
	instanceCmdTimeout = 50 * time.Millisecond
	t.Cleanup(func() { instanceCmdTimeout = prev })

	// The owner applies the change, then withholds its reply.
	var applied, queue atomic.Int32
	_, err := nc.Subscribe(ec2v1.InstanceCommandSubject(metaTestID), func(*nats.Msg) { applied.Add(1) })
	require.NoError(t, err)
	queueReply(t, nc, metaOutputJSON(t, 3), &queue)

	_, err = ModifyInstanceMetadataOptions(context.Background(), metaInput(3), nc, "acct")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
	assert.Equal(t, int32(1), applied.Load(), "the owner saw the command, so the outcome is unknown")
	assert.Zero(t, queue.Load(), "a timeout never falls back to the queue group")
}
