// Drives ReceiveMessage and DeleteMessage through the signed gateway against
// the real queue and message owners, with a test receipt key ring and the
// message store on its own embedded NATS server.

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/mulgadc/spinifex/internal/testkit"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	sqsawsapi "github.com/mulgadc/spinifex/spinifex/domains/sqs/awsapi"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/queue"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	receiveTarget = "AmazonSQS.ReceiveMessage"
	deleteTarget  = "AmazonSQS.DeleteMessage"
)

var gwReceiptKeys = func() *message.ReceiptKeys {
	k, err := message.NewReceiptKeys(1, map[uint32][]byte{1: bytes.Repeat([]byte{9}, message.MinReceiptKeyLen)})
	if err != nil {
		panic(err)
	}
	return k
}()

// countingOwner counts calls reaching the receive and delete owners.
type countingOwner struct {
	recv sqsawsapi.Receives
	del  sqsawsapi.Deletes
	vis  sqsawsapi.Visibility

	mu       sync.Mutex
	receives int
	deletes  int
	changes  int
}

func (c *countingOwner) Receive(ctx context.Context, in message.ReceiveInput) ([]message.Received, error) {
	c.mu.Lock()
	c.receives++
	c.mu.Unlock()
	return c.recv.Receive(ctx, in)
}

func (c *countingOwner) Delete(ctx context.Context, in message.DeleteInput) error {
	c.mu.Lock()
	c.deletes++
	del := c.del
	c.mu.Unlock()
	return del.Delete(ctx, in)
}

func (c *countingOwner) ChangeVisibility(ctx context.Context, in message.ChangeVisibilityInput) error {
	c.mu.Lock()
	c.changes++
	vis := c.vis
	c.mu.Unlock()
	return vis.ChangeVisibility(ctx, in)
}

func (c *countingOwner) changeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.changes
}

func (c *countingOwner) counts() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.receives, c.deletes
}

type sqsRecvApp struct {
	sqsApp

	msgs  message.Store
	owner *countingOwner
}

// newSQSRecvApp serves send, receive and delete on one message store; wrap,
// when set, replaces the store receives and deletes read and write through.
func newSQSRecvApp(t *testing.T, docs []iamdomain.PolicyDocument, wrap func(message.Store) message.Store) sqsRecvApp {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	qs, err := queue.Open(context.Background(), js, 3, 2)
	require.NoError(t, err)
	ms := openMessageStore(t)
	via := ms
	if wrap != nil {
		via = wrap(ms)
	}
	owner := &countingOwner{
		recv: &message.Receiver{Store: via, Queues: qs, Keys: gwReceiptKeys},
		del:  &message.Deleter{Store: via, Queues: qs, Keys: gwReceiptKeys},
		vis:  &message.VisibilityChanger{Store: via, Queues: qs, Keys: gwReceiptKeys},
	}
	var app http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { app.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	_, app = dispatchSeamApp(t, docs, sqsawsapi.NewRegistration(sqsawsapi.Deps{
		Queues: qs, Messages: &message.Sender{Store: ms, Queues: qs},
		Receives: owner, Deletes: owner, Visibility: owner, Endpoint: srv.URL,
	}))
	return sqsRecvApp{sqsApp: sqsApp{url: srv.URL, store: qs, cfg: sdkConfig(srv.URL)}, msgs: ms, owner: owner}
}

func (a sqsRecvApp) create(t *testing.T, name string, attrs map[string]string) string {
	t.Helper()
	out, err := a.client().CreateQueue(context.Background(), &sqs.CreateQueueInput{QueueName: aws.String(name), Attributes: attrs})
	require.NoError(t, err)
	return *out.QueueUrl
}

func (a sqsRecvApp) send(t *testing.T, u, body string) *sqs.SendMessageOutput {
	t.Helper()
	out, err := a.client().SendMessage(context.Background(), &sqs.SendMessageInput{QueueUrl: aws.String(u), MessageBody: aws.String(body)})
	require.NoError(t, err)
	return out
}

func (a sqsRecvApp) receive(t *testing.T, in *sqs.ReceiveMessageInput) []sqstypes.Message {
	t.Helper()
	out, err := a.client().ReceiveMessage(context.Background(), in)
	require.NoError(t, err)
	return out.Messages
}

func recvBody(u, rest string) []byte {
	return []byte(`{"QueueUrl":"` + u + `"` + rest + `}`)
}

func deleteBody(u, handle string) []byte {
	return []byte(`{"QueueUrl":"` + u + `","ReceiptHandle":"` + handle + `"}`)
}

var (
	wireInvalidHandle = wireError{400, "com.amazonaws.sqs#ReceiptHandleIsInvalid", "ReceiptHandleIsInvalid;Sender", ""}
	wireUnavailable   = wireError{503, "com.amazon.coral.service#ServiceUnavailableException", "ServiceUnavailable;Receiver", ""}
	wireUnsupported   = wireError{400, "com.amazonaws.sqs#UnsupportedOperation", "AWS.SimpleQueueService.UnsupportedOperation;Sender", ""}
	wireInvalidParam  = wireError{400, "com.amazon.coral.service#InvalidParameterValueException", "InvalidParameterValue;Sender", ""}
	wireMissingParam  = wireError{400, "com.amazon.coral.service#MissingRequiredParameterException", "MissingParameter;Sender", ""}
	wireDenied        = wireError{403, "com.amazon.coral.service#AccessDeniedException", "AccessDenied;Sender", ""}
)

// The SDK's own MD5 check runs on every receive below, so a wrong
// MD5OfBody fails the call rather than passing silently.
func TestSQSReceiveMessage_SendReceiveDelete(t *testing.T) {
	a := newSQSRecvApp(t, allowAllECRDocs(), nil)
	u := a.create(t, "orders", nil)
	before := time.Now().UnixMilli()
	sent := a.send(t, u, "hello")

	got := a.receive(t, &sqs.ReceiveMessageInput{QueueUrl: aws.String(u),
		MessageSystemAttributeNames: []sqstypes.MessageSystemAttributeName{sqstypes.MessageSystemAttributeNameAll}})
	require.Len(t, got, 1)
	m := got[0]
	assert.Equal(t, aws.ToString(sent.MessageId), aws.ToString(m.MessageId))
	assert.Equal(t, "hello", aws.ToString(m.Body))
	assert.Equal(t, md5Hex([]byte("hello")), aws.ToString(m.MD5OfBody))
	assert.NotEmpty(t, aws.ToString(m.ReceiptHandle))
	assert.Equal(t, aliceUserID, m.Attributes["SenderId"])
	assert.Equal(t, "1", m.Attributes["ApproximateReceiveCount"])
	for _, k := range []string{"SentTimestamp", "ApproximateFirstReceiveTimestamp"} {
		ms, err := strconv.ParseInt(m.Attributes[k], 10, 64)
		require.NoError(t, err, k)
		assert.GreaterOrEqual(t, ms, before, k)
	}
	assert.Len(t, m.Attributes, 4, "only attributes a message can carry are returned")
	assert.Empty(t, m.MessageAttributes)

	assert.Empty(t, a.receive(t, &sqs.ReceiveMessageInput{QueueUrl: aws.String(u)}), "the message is in flight")
	_, err := a.client().DeleteMessage(context.Background(), &sqs.DeleteMessageInput{QueueUrl: aws.String(u), ReceiptHandle: m.ReceiptHandle})
	require.NoError(t, err)
	_, err = a.client().DeleteMessage(context.Background(), &sqs.DeleteMessageInput{QueueUrl: aws.String(u), ReceiptHandle: m.ReceiptHandle})
	require.NoError(t, err, "a repeated delete succeeds")
	assert.Empty(t, a.receive(t, &sqs.ReceiveMessageInput{QueueUrl: aws.String(u), VisibilityTimeout: 1}))
}

func TestSQSReceiveMessage_MaxNumberOfMessages(t *testing.T) {
	a := newSQSRecvApp(t, allowAllECRDocs(), nil)
	u := a.create(t, "orders", nil)
	for i := range 12 {
		a.send(t, u, "m"+strconv.Itoa(i))
	}
	for _, n := range []string{"0", "11", "-1"} {
		assert.Equal(t, wireInvalidParam, withoutMessage(readWireError(t, a.raw(t, receiveTarget, recvBody(u, `,"MaxNumberOfMessages":`+n)))), n)
	}
	r, _ := a.owner.counts()
	assert.Equal(t, 0, r, "an out-of-range request never reaches the owner")

	assert.Len(t, a.receive(t, &sqs.ReceiveMessageInput{QueueUrl: aws.String(u)}), 1, "the default is one")
	ten := a.receive(t, &sqs.ReceiveMessageInput{QueueUrl: aws.String(u), MaxNumberOfMessages: 10})
	assert.Len(t, ten, 10)
	assert.Len(t, a.receive(t, &sqs.ReceiveMessageInput{QueueUrl: aws.String(u), MaxNumberOfMessages: 10}), 1, "only the last message is left")
	seen := map[string]bool{}
	for _, m := range ten {
		seen[aws.ToString(m.MessageId)] = true
	}
	assert.Len(t, seen, 10, "no message is returned twice")
}

// The queue's ReceiveMessageWaitTimeSeconds is a long poll when the request
// omits WaitTimeSeconds; an explicit zero is a short poll.
func TestSQSReceiveMessage_LongPollQueueDefault(t *testing.T) {
	a := newSQSRecvApp(t, allowAllECRDocs(), nil)
	u := a.create(t, "polled", map[string]string{"ReceiveMessageWaitTimeSeconds": "1"})

	start := time.Now()
	assert.Empty(t, a.receive(t, &sqs.ReceiveMessageInput{QueueUrl: aws.String(u)}))
	assert.GreaterOrEqual(t, time.Since(start), time.Second, "the queue default is never a silent short poll")

	// The SDK's WaitTimeSeconds is a plain int32 that omits zero, so an SDK
	// zero still takes the queue default; only the wire can send zero.
	start = time.Now()
	assert.Empty(t, a.receive(t, &sqs.ReceiveMessageInput{QueueUrl: aws.String(u), WaitTimeSeconds: 0}))
	assert.GreaterOrEqual(t, time.Since(start), time.Second)

	start = time.Now()
	resp := a.raw(t, receiveTarget, recvBody(u, `,"WaitTimeSeconds":0`))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()
	assert.Less(t, time.Since(start), time.Second, "an explicit zero overrides the queue")

	assert.Equal(t, wireInvalidParam, withoutMessage(readWireError(t, a.raw(t, receiveTarget, recvBody(u, `,"WaitTimeSeconds":21`)))))
}

// twin serves the same queue and message stores through a second gateway
// with its own receiver, as a second node would.
func (a sqsRecvApp) twin(t *testing.T) sqsRecvApp {
	t.Helper()
	owner := &countingOwner{
		recv: &message.Receiver{Store: a.msgs, Queues: a.store, Keys: gwReceiptKeys},
		del:  &message.Deleter{Store: a.msgs, Queues: a.store, Keys: gwReceiptKeys},
		vis:  &message.VisibilityChanger{Store: a.msgs, Queues: a.store, Keys: gwReceiptKeys},
	}
	var app http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { app.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	_, app = dispatchSeamApp(t, allowAllECRDocs(), sqsawsapi.NewRegistration(sqsawsapi.Deps{
		Queues: a.store, Messages: &message.Sender{Store: a.msgs, Queues: a.store},
		Receives: owner, Deletes: owner, Visibility: owner, Endpoint: srv.URL,
	}))
	return sqsRecvApp{sqsApp: sqsApp{url: srv.URL, store: a.store, cfg: sdkConfig(srv.URL)}, msgs: a.msgs, owner: owner}
}

type sdkReceive struct {
	got  []sqstypes.Message
	err  error
	took time.Duration
}

func (a sqsRecvApp) goReceive(ctx context.Context, in *sqs.ReceiveMessageInput) <-chan sdkReceive {
	out := make(chan sdkReceive, 1)
	go func() {
		start := time.Now()
		res, err := a.client().ReceiveMessage(ctx, in)
		r := sdkReceive{err: err, took: time.Since(start)}
		if res != nil {
			r.got = res.Messages
		}
		out <- r
	}()
	return out
}

// Waiters on two gateways, each with its own receiver: a send through one
// wakes them, and exactly one takes the message, then deletes it.
func TestSQSReceiveMessage_LongPollAcrossReceivers(t *testing.T) {
	a := newSQSRecvApp(t, allowAllECRDocs(), nil)
	b := a.twin(t)
	u := a.create(t, "orders", nil)
	ub := b.url + u[len(a.url):]

	ra := a.goReceive(context.Background(), &sqs.ReceiveMessageInput{QueueUrl: aws.String(u), WaitTimeSeconds: 5})
	rb := b.goReceive(context.Background(), &sqs.ReceiveMessageInput{QueueUrl: aws.String(ub), WaitTimeSeconds: 5})
	time.Sleep(300 * time.Millisecond)
	sent := a.send(t, u, "wake")

	x, y := <-ra, <-rb
	require.NoError(t, x.err)
	require.NoError(t, y.err)
	got := append(x.got, y.got...)
	require.Len(t, got, 1, "exactly one waiter takes the message")
	assert.Equal(t, aws.ToString(sent.MessageId), aws.ToString(got[0].MessageId))
	assert.Equal(t, "wake", aws.ToString(got[0].Body))
	assert.Less(t, min(x.took, y.took), 3*time.Second, "the taker was woken by the send")

	taker, url := a, u
	if len(y.got) == 1 {
		taker, url = b, ub
	}
	_, err := taker.client().DeleteMessage(context.Background(), &sqs.DeleteMessageInput{QueueUrl: aws.String(url), ReceiptHandle: got[0].ReceiptHandle})
	require.NoError(t, err)
}

// A client that gives up ends its long poll promptly.
func TestSQSReceiveMessage_LongPollClientCancel(t *testing.T) {
	a := newSQSRecvApp(t, allowAllECRDocs(), nil)
	u := a.create(t, "orders", nil)
	ctx, cancel := context.WithCancel(context.Background())
	res := a.goReceive(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(u), WaitTimeSeconds: 20})
	time.Sleep(300 * time.Millisecond)
	cancel()
	r := <-res
	require.ErrorIs(t, r.err, context.Canceled)
	assert.Less(t, r.took, 2*time.Second)

	// The gateway's receive has ended too: a later send is taken by a new poll.
	a.send(t, u, "after")
	got := a.receive(t, &sqs.ReceiveMessageInput{QueueUrl: aws.String(u)})
	require.Len(t, got, 1)
}

func TestSQSReceiveMessage_RequestValidation(t *testing.T) {
	a := newSQSRecvApp(t, allowAllECRDocs(), nil)
	u := a.create(t, "orders", nil)
	a.send(t, u, "hello")
	for name, tc := range map[string]struct {
		target string
		body   []byte
		want   wireError
	}{
		"receive no queue url":     {receiveTarget, []byte(`{}`), wireMissingParam},
		"receive attempt id":       {receiveTarget, recvBody(u, `,"ReceiveRequestAttemptId":"x"`), wireUnsupported},
		"receive empty attempt id": {receiveTarget, recvBody(u, `,"ReceiveRequestAttemptId":""`), wireUnsupported},
		"receive visibility over":  {receiveTarget, recvBody(u, `,"VisibilityTimeout":43201`), wireInvalidParam},
		"receive trailing input": {receiveTarget, append(recvBody(u, ""), '}'),
			wireError{400, "com.amazon.coral.service#SerializationException", "MalformedInput;Sender", ""}},
		"receive wrong host": {receiveTarget, recvBody(strings.Replace(u, "127.0.0.1", "localhost", 1), ""),
			wireError{404, "com.amazonaws.sqs#InvalidAddress", "InvalidAddress;Sender", ""}},
		"receive other account": {receiveTarget, recvBody(a.url+"/999988887777/orders", ""),
			wireError{404, "com.amazonaws.sqs#InvalidAddress", "InvalidAddress;Sender", ""}},
		"delete no handle":    {deleteTarget, []byte(`{"QueueUrl":"` + u + `"}`), wireMissingParam},
		"delete empty handle": {deleteTarget, deleteBody(u, ""), wireMissingParam},
		"delete no queue url": {deleteTarget, []byte(`{"ReceiptHandle":"x"}`), wireMissingParam},
		"delete other account": {deleteTarget, deleteBody(a.url+"/999988887777/orders", "x"),
			wireError{404, "com.amazonaws.sqs#InvalidAddress", "InvalidAddress;Sender", ""}},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, withoutMessage(readWireError(t, a.raw(t, tc.target, tc.body))))
		})
	}
	r, d := a.owner.counts()
	assert.Equal(t, 0, r+d, "invalid requests never reach the owner")
}

func TestSQSDeleteMessage_Handles(t *testing.T) {
	a := newSQSRecvApp(t, allowAllECRDocs(), nil)
	u := a.create(t, "orders", nil)
	other := a.create(t, "other", nil)
	a.send(t, u, "hello")
	first := a.receive(t, &sqs.ReceiveMessageInput{QueueUrl: aws.String(u), VisibilityTimeout: 1})
	require.Len(t, first, 1)
	old := aws.ToString(first[0].ReceiptHandle)

	tampered := []byte(old)
	mid := len(tampered) / 2
	tampered[mid] = map[bool]byte{true: 'B', false: 'A'}[tampered[mid] == 'A']
	for name, tc := range map[string]struct{ u, h string }{
		"tampered":    {u, string(tampered)},
		"garbage":     {u, "not-a-handle"},
		"other queue": {other, old},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, wireInvalidHandle, withoutMessage(readWireError(t, a.raw(t, deleteTarget, deleteBody(tc.u, tc.h)))))
		})
	}
	_, err := a.client().DeleteMessage(context.Background(), &sqs.DeleteMessageInput{QueueUrl: aws.String(u), ReceiptHandle: aws.String("not-a-handle")})
	var invalid *sqstypes.ReceiptHandleIsInvalid
	assert.ErrorAs(t, err, &invalid)

	require.Eventually(t, func() bool {
		return len(a.receive(t, &sqs.ReceiveMessageInput{QueueUrl: aws.String(u), VisibilityTimeout: 1})) == 1
	}, 5*time.Second, 100*time.Millisecond, "re-received once its visibility lapses")
	resp := a.raw(t, deleteTarget, deleteBody(u, old))
	require.Equal(t, http.StatusOK, resp.StatusCode, "a stale handle succeeds without deleting the newer receive")
	require.Eventually(t, func() bool {
		return len(a.receive(t, &sqs.ReceiveMessageInput{QueueUrl: aws.String(u)})) == 1
	}, 5*time.Second, 100*time.Millisecond, "the message survived the stale delete")
}

func TestSQSReceiveMessage_QueueIsolation(t *testing.T) {
	a := newSQSRecvApp(t, allowAllECRDocs(), nil)
	u1 := a.create(t, "one", nil)
	u2 := a.create(t, "two", nil)
	a.send(t, u1, "for-one")
	assert.Empty(t, a.receive(t, &sqs.ReceiveMessageInput{QueueUrl: aws.String(u2), MaxNumberOfMessages: 10}))
	got := a.receive(t, &sqs.ReceiveMessageInput{QueueUrl: aws.String(u1), MaxNumberOfMessages: 10})
	require.Len(t, got, 1)
	assert.Equal(t, "for-one", aws.ToString(got[0].Body))
}

func TestSQSReceiveMessage_Authorization(t *testing.T) {
	queueARN := "arn:aws:sqs:" + testRegion + ":" + ecrTestAccount + ":orders"
	setup := sqsPolicy([]string{"sqs:CreateQueue", "sqs:SendMessage"}, "*")
	for name, tc := range map[string]struct {
		docs          []iamdomain.PolicyDocument
		recvOK, delOK bool
	}{
		"other actions": {append(setup, sqsPolicy([]string{"sqs:ChangeMessageVisibility"}, "*")...), false, false},
		"receive only":  {append(setup, sqsPolicy([]string{"sqs:ReceiveMessage"}, queueARN)...), true, false},
		"other queue":   {append(setup, sqsPolicy([]string{"sqs:ReceiveMessage", "sqs:DeleteMessage"}, queueARN+"-other")...), false, false},
		"both here":     {append(setup, sqsPolicy([]string{"sqs:ReceiveMessage", "sqs:DeleteMessage"}, queueARN)...), true, true},
		"explicit deny": {append(sqsPolicy([]string{"sqs:*"}, "*"), iamdomain.PolicyDocument{Version: "2012-10-17", Statement: []iamdomain.Statement{
			{Effect: "Deny", Action: iamdomain.StringOrArr{"sqs:ReceiveMessage", "sqs:DeleteMessage"}, Resource: iamdomain.StringOrArr{queueARN + "*"}},
		}}), false, false},
	} {
		t.Run(name, func(t *testing.T) {
			a := newSQSRecvApp(t, tc.docs, nil)
			u := a.create(t, "orders", nil)
			a.send(t, u, "hello")
			absent := a.url + "/" + ecrTestAccount + "/orders-absent"

			resp := a.raw(t, receiveTarget, recvBody(u, ""))
			handle := "x"
			if tc.recvOK {
				require.Equal(t, http.StatusOK, resp.StatusCode)
				var out struct {
					Messages []struct{ ReceiptHandle string }
				}
				require.NoError(t, jsonDecode(resp, &out))
				require.Len(t, out.Messages, 1)
				handle = out.Messages[0].ReceiptHandle
			} else {
				assert.Equal(t, wireDenied, withoutMessage(readWireError(t, resp)))
				assert.Equal(t, wireDenied.typ, readWireError(t, a.raw(t, receiveTarget, recvBody(absent, ""))).typ, "denial does not reveal whether a queue exists")
			}
			resp = a.raw(t, deleteTarget, deleteBody(u, handle))
			if tc.delOK {
				assert.Equal(t, http.StatusOK, resp.StatusCode)
			} else {
				assert.Equal(t, wireDenied, withoutMessage(readWireError(t, resp)), "a handle is not checked before authorization")
				assert.Equal(t, wireDenied.typ, readWireError(t, a.raw(t, deleteTarget, deleteBody(absent, handle))).typ)
			}
			r, d := a.owner.counts()
			assert.Equal(t, map[bool]int{true: 1, false: 0}[tc.recvOK], r)
			assert.Equal(t, map[bool]int{true: 1, false: 0}[tc.delOK], d)
		})
	}
}

// recvFaultStore fails state writes; landed makes the write apply before the
// error, as a lost acknowledgement does. scan fails every scan.
type recvFaultStore struct {
	message.Store

	landed, scan bool
}

var errStateLost = errors.New("nats: timeout")

func (f recvFaultStore) PutState(ctx context.Context, ns message.Namespace, pos message.Position, v []byte, exp message.Revision) (message.Revision, error) {
	if f.scan {
		return f.Store.PutState(ctx, ns, pos, v, exp)
	}
	if f.landed {
		if _, err := f.Store.PutState(ctx, ns, pos, v, exp); err != nil {
			return 0, err
		}
	}
	return 0, errStateLost
}

func (f recvFaultStore) Scan(ctx context.Context, sh message.Shard, from message.ScanCursor, limit int) (message.ScanPage, error) {
	if f.scan {
		return message.ScanPage{}, errStateLost
	}
	return f.Store.Scan(ctx, sh, from, limit)
}

func TestSQSReceiveMessage_StoreFailureIsNeverEmptyOrSuccess(t *testing.T) {
	for name, f := range map[string]recvFaultStore{
		"scan failed":            {scan: true},
		"claim failed":           {},
		"claim reply lost after": {landed: true},
	} {
		t.Run(name, func(t *testing.T) {
			a := newSQSRecvApp(t, allowAllECRDocs(), func(s message.Store) message.Store { f.Store = s; return f })
			u := a.create(t, "orders", nil)
			a.send(t, u, "hello")
			assert.Equal(t, wireUnavailable, withoutMessage(readWireError(t, a.raw(t, receiveTarget, recvBody(u, "")))))
		})
	}
}

func TestSQSDeleteMessage_StoreFailureIsNeverSuccess(t *testing.T) {
	for name, landed := range map[string]bool{"tombstone failed": false, "tombstone reply lost after landing": true} {
		t.Run(name, func(t *testing.T) {
			a := newSQSRecvApp(t, allowAllECRDocs(), nil)
			u := a.create(t, "orders", nil)
			a.send(t, u, "hello")
			got := a.receive(t, &sqs.ReceiveMessageInput{QueueUrl: aws.String(u)})
			require.Len(t, got, 1)
			a.owner.mu.Lock()
			a.owner.del = &message.Deleter{Store: recvFaultStore{Store: a.msgs, landed: landed}, Queues: a.store, Keys: gwReceiptKeys}
			a.owner.mu.Unlock()
			resp := a.raw(t, deleteTarget, deleteBody(u, aws.ToString(got[0].ReceiptHandle)))
			q, err := a.store.Resolve(context.Background(), ecrTestAccount, testRegion, "orders")
			require.NoError(t, err)
			st, err := a.msgs.GetState(context.Background(), q.Namespace(), message.Position(1))
			require.NoError(t, err)
			tombstoned := strings.Contains(string(st.Value), `"s":"deleted"`)
			if !landed {
				assert.Equal(t, wireUnavailable, withoutMessage(readWireError(t, resp)))
				assert.False(t, tombstoned)
				return
			}
			// The re-read confirms this handle's tombstone landed, so the
			// outcome is settled rather than unknown.
			assert.Equal(t, http.StatusOK, resp.StatusCode)
			assert.True(t, tombstoned)
		})
	}
}

// No body, receipt handle or credential reaches a log line or an error.
func TestSQSReceiveMessage_LogsCarryNoBodyHandleOrCredential(t *testing.T) {
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const marker = "zq-recv-marker-5a1e"
	a := newSQSRecvApp(t, allowAllECRDocs(), nil)
	u := a.create(t, "orders", nil)
	other := a.create(t, "other", nil)
	a.send(t, u, marker)
	got := a.receive(t, &sqs.ReceiveMessageInput{QueueUrl: aws.String(u)})
	require.Len(t, got, 1)
	handle := aws.ToString(got[0].ReceiptHandle)

	var errText bytes.Buffer
	for _, req := range []struct {
		target string
		body   []byte
	}{
		{deleteTarget, deleteBody(other, handle)},
		{deleteTarget, deleteBody(u, handle+"x")},
		{receiveTarget, recvBody(u, `,"WaitTimeSeconds":21`)},
		{receiveTarget, recvBody(a.url+"/"+ecrTestAccount+"/absent", "")},
	} {
		errText.WriteString(readWireError(t, a.raw(t, req.target, req.body)).message)
	}
	require.Positive(t, buf.Len(), "the failure paths must have logged something to inspect")
	for _, secret := range []string{marker, handle, testSecretKey} {
		assert.NotContains(t, buf.String(), secret)
		assert.NotContains(t, errText.String(), secret)
	}
}

func jsonDecode(resp *http.Response, v any) error {
	defer func() { _ = resp.Body.Close() }()
	return json.NewDecoder(resp.Body).Decode(v)
}
