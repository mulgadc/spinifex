package message_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	sqsjs "github.com/mulgadc/spinifex/spinifex/domains/sqs/message/jetstream"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/queue"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	lcAccount = "000000000001"
	lcRegion  = "ap-southeast-2"
)

// owners is the queue store and the four message owners a poller drives,
// over a real JetStream message store.
type owners struct {
	queues *queue.Store
	send   *message.Sender
	recv   *message.Receiver
	del    *message.Deleter
	vis    *message.VisibilityChanger
}

func newOwners(t *testing.T) owners {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	ctx := context.Background()
	qs, err := queue.Open(ctx, js, 3, 2)
	require.NoError(t, err)
	ms, err := sqsjs.New(ctx, js, sqsjs.Config{MessageStreams: 3, StateBuckets: 2, DuplicateWindow: time.Minute})
	require.NoError(t, err)
	keys, err := message.NewReceiptKeys(1, map[uint32][]byte{1: bytes.Repeat([]byte{9}, message.MinReceiptKeyLen)})
	require.NoError(t, err)
	return owners{
		queues: qs,
		send:   &message.Sender{Store: ms, Queues: qs},
		recv:   &message.Receiver{Store: ms, Queues: qs, Keys: keys},
		del:    &message.Deleter{Store: ms, Queues: qs, Keys: keys},
		vis:    &message.VisibilityChanger{Store: ms, Queues: qs, Keys: keys},
	}
}

func (o owners) create(t *testing.T, name string) {
	t.Helper()
	_, err := o.queues.Create(context.Background(), queue.CreateRequest{Account: lcAccount, Region: lcRegion, Name: name,
		Attributes: map[string]string{"VisibilityTimeout": "30"}})
	require.NoError(t, err)
}

func (o owners) put(t *testing.T, name, body string) {
	t.Helper()
	_, err := o.send.Send(context.Background(), message.SendInput{Account: lcAccount, Region: lcRegion, Queue: name,
		Sender: "AIDAPOLLER", Body: []byte(body)})
	require.NoError(t, err)
}

func (o owners) receive(t *testing.T, name string, n, wait int) []message.Received {
	t.Helper()
	got, err := o.recv.Receive(context.Background(), message.ReceiveInput{Account: lcAccount, Region: lcRegion, Queue: name,
		MaxNumberOfMessages: &n, WaitTimeSeconds: &wait})
	require.NoError(t, err)
	return got
}

func (o owners) changeVisibility(name, handle string, timeout int) error {
	return o.vis.ChangeVisibility(context.Background(), message.ChangeVisibilityInput{Account: lcAccount, Region: lcRegion,
		Queue: name, ReceiptHandle: handle, VisibilityTimeout: timeout})
}

func (o owners) delete(name, handle string) error {
	return o.del.Delete(context.Background(), message.DeleteInput{Account: lcAccount, Region: lcRegion, Queue: name, ReceiptHandle: handle})
}

// A poller receives a batch, returns one record for retry and deletes the
// rest; the retried record comes back with a new handle and a higher count.
func TestPollerLifecycle(t *testing.T) {
	o := newOwners(t)
	o.create(t, "jobs")
	o.put(t, "jobs", "a")
	o.put(t, "jobs", "b")

	first := o.receive(t, "jobs", 10, 0)
	require.Len(t, first, 2)
	for _, m := range first {
		assert.Equal(t, uint64(1), m.ReceiveCount)
		assert.Equal(t, "AIDAPOLLER", m.SenderID)
		assert.False(t, m.FirstReceive.IsZero())
	}
	assert.Empty(t, o.receive(t, "jobs", 10, 0), "both are in flight")

	retry, done := first[0], first[1]
	require.NoError(t, o.changeVisibility("jobs", retry.ReceiptHandle, 0))
	require.NoError(t, o.delete("jobs", done.ReceiptHandle))

	again := o.receive(t, "jobs", 10, 0)
	require.Len(t, again, 1, "only the released record is redelivered")
	assert.Equal(t, retry.MessageID, again[0].MessageID)
	assert.Equal(t, retry.Body, again[0].Body)
	assert.Equal(t, uint64(2), again[0].ReceiveCount)
	assert.Equal(t, retry.FirstReceive, again[0].FirstReceive, "first receive is kept across redelivery")
	assert.NotEqual(t, retry.ReceiptHandle, again[0].ReceiptHandle)

	assert.Error(t, o.changeVisibility("jobs", retry.ReceiptHandle, 0), "the superseded handle cannot release the new receive")

	require.NoError(t, o.delete("jobs", again[0].ReceiptHandle))
	require.NoError(t, o.delete("jobs", again[0].ReceiptHandle), "repeating a delete succeeds")
	assert.Error(t, o.changeVisibility("jobs", again[0].ReceiptHandle, 10), "a deleted message is no longer in flight")
	assert.Empty(t, o.receive(t, "jobs", 10, 0))
}

// Extending visibility keeps a slow record hidden from other pollers.
func TestExtendVisibilityKeepsMessageHidden(t *testing.T) {
	o := newOwners(t)
	o.create(t, "slow")
	o.put(t, "slow", "work")
	got := o.receive(t, "slow", 1, 0)
	require.Len(t, got, 1)

	require.NoError(t, o.changeVisibility("slow", got[0].ReceiptHandle, 600))
	assert.Empty(t, o.receive(t, "slow", 10, 0))
	assert.ErrorIs(t, o.changeVisibility("slow", got[0].ReceiptHandle, 43201), message.ErrInvalidParameter)
}

// A handle only acts on the queue that issued it.
func TestHandleIsBoundToItsQueue(t *testing.T) {
	o := newOwners(t)
	o.create(t, "a")
	o.create(t, "b")
	o.put(t, "a", "x")
	o.put(t, "b", "y")
	fromA := o.receive(t, "a", 1, 0)
	require.Len(t, fromA, 1)

	assert.ErrorIs(t, o.delete("b", fromA[0].ReceiptHandle), message.ErrReceiptInvalid)
	assert.ErrorIs(t, o.changeVisibility("b", fromA[0].ReceiptHandle, 0), message.ErrReceiptInvalid)
	assert.ErrorIs(t, o.delete("a", "not-a-handle"), message.ErrReceiptInvalid)
	got := o.receive(t, "b", 1, 0)
	require.Len(t, got, 1, "queue b's message was untouched")
	assert.Equal(t, []byte("y"), got[0].Body)
}

// A long poll returns waiting messages at once and an empty poll waits.
func TestLongPoll(t *testing.T) {
	o := newOwners(t)
	o.create(t, "events")

	start := time.Now()
	assert.Empty(t, o.receive(t, "events", 10, 1))
	assert.GreaterOrEqual(t, time.Since(start), time.Second)

	o.put(t, "events", "ready")
	start = time.Now()
	got := o.receive(t, "events", 10, 20)
	require.Len(t, got, 1)
	assert.Less(t, time.Since(start), 10*time.Second, "a waiting message ends the poll early")

	_, err := o.recv.Receive(context.Background(), message.ReceiveInput{Account: lcAccount, Region: lcRegion, Queue: "missing"})
	assert.ErrorIs(t, err, message.ErrQueueDoesNotExist)
}
