// Drives SendMessage through the signed gateway against the real queue owner
// and message owner, with the message store on its own embedded NATS server.

package gateway

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
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
	sqsjs "github.com/mulgadc/spinifex/spinifex/domains/sqs/message/jetstream"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/queue"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sendTarget = "AmazonSQS.SendMessage"

// The authenticated caller in the seam app is IAM user alice.
const aliceUserID = "AIDAALICE"

// recordingMessages delegates to the real sender and keeps every outcome, so
// a test can read the stored message back and count owner calls.
type recordingMessages struct {
	next  sqsawsapi.Messages
	mu    sync.Mutex
	calls int
	sent  []message.SendResult
}

func (r *recordingMessages) Send(ctx context.Context, in message.SendInput) (message.SendResult, error) {
	res, err := r.next.Send(ctx, in)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if err == nil {
		r.sent = append(r.sent, res)
	}
	return res, err
}

type sqsSendApp struct {
	sqsApp

	msgs  message.Store
	owner *recordingMessages
}

func openMessageStore(t *testing.T) message.Store {
	t.Helper()
	ns, err := server.NewServer(&server.Options{
		Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(),
		NoLog: true, NoSigs: true, MaxPayload: 2 << 20,
	})
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(10*time.Second))
	t.Cleanup(ns.Shutdown)
	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	ms, err := sqsjs.New(context.Background(), js, sqsjs.Config{MessageStreams: 3, StateBuckets: 2, DuplicateWindow: time.Minute})
	require.NoError(t, err)
	return ms
}

// newSQSSendApp serves the registration with a real sender; wrap, when set,
// replaces the message store the sender appends through.
func newSQSSendApp(t *testing.T, docs []iamdomain.PolicyDocument, wrap func(message.Store) message.Store) sqsSendApp {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	qs, err := queue.Open(context.Background(), js, 3, 2)
	require.NoError(t, err)
	ms := openMessageStore(t)
	appendVia := ms
	if wrap != nil {
		appendVia = wrap(ms)
	}
	owner := &recordingMessages{next: &message.Sender{Store: appendVia, Queues: qs}}
	var app http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { app.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	_, app = dispatchSeamApp(t, docs, sqsawsapi.NewRegistration(sqsawsapi.Deps{Queues: qs, Messages: owner, Endpoint: srv.URL}))
	return sqsSendApp{sqsApp: sqsApp{url: srv.URL, store: qs, cfg: sdkConfig(srv.URL)}, msgs: ms, owner: owner}
}

func (a sqsSendApp) create(t *testing.T, name string, attrs map[string]string) (string, queue.Record) {
	t.Helper()
	out, err := a.client().CreateQueue(context.Background(), &sqs.CreateQueueInput{QueueName: aws.String(name), Attributes: attrs})
	require.NoError(t, err)
	rec, err := a.store.Resolve(context.Background(), ecrTestAccount, testRegion, name)
	require.NoError(t, err)
	return *out.QueueUrl, rec
}

func (a sqsSendApp) stored(t *testing.T, q queue.Record, i int) message.Message {
	t.Helper()
	a.owner.mu.Lock()
	res := a.owner.sent[i]
	a.owner.mu.Unlock()
	m, err := a.msgs.Get(context.Background(), q.Namespace(), res.Position)
	require.NoError(t, err)
	return m
}

func (a sqsSendApp) calls() int {
	a.owner.mu.Lock()
	defer a.owner.mu.Unlock()
	return a.owner.calls
}

func md5Hex(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

func sendBody(u, rest string) []byte {
	return []byte(`{"QueueUrl":"` + u + `","MessageBody":"hello"` + rest + `}`)
}

func TestSQSSendMessage_SDKStoresBodyAndCallerSender(t *testing.T) {
	a := newSQSSendApp(t, allowAllECRDocs(), nil)
	u, q := a.create(t, "orders", nil)

	out, err := a.client().SendMessage(context.Background(), &sqs.SendMessageInput{QueueUrl: aws.String(u), MessageBody: aws.String("hello")})
	require.NoError(t, err)
	assert.Regexp(t, `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`, aws.ToString(out.MessageId))
	assert.Equal(t, md5Hex([]byte("hello")), aws.ToString(out.MD5OfMessageBody))

	m := a.stored(t, q, 0)
	assert.Equal(t, []byte("hello"), m.Body)
	assert.Equal(t, aws.ToString(out.MessageId), m.Meta[message.MetaID])
	assert.Equal(t, aliceUserID, m.Meta[message.MetaSender])
	assert.Equal(t, ecrTestAccount, m.Meta[message.MetaAcct])
}

func TestSQSSendMessage_FullBodyAndOversize(t *testing.T) {
	a := newSQSSendApp(t, allowAllECRDocs(), nil)
	u, q := a.create(t, "big", nil)
	full := strings.Repeat("a", message.MaxMessageSize)

	out, err := a.client().SendMessage(context.Background(), &sqs.SendMessageInput{QueueUrl: aws.String(u), MessageBody: aws.String(full)})
	require.NoError(t, err)
	assert.Equal(t, md5Hex([]byte(full)), aws.ToString(out.MD5OfMessageBody))
	assert.Len(t, a.stored(t, q, 0).Body, message.MaxMessageSize)

	_, err = a.client().SendMessage(context.Background(), &sqs.SendMessageInput{QueueUrl: aws.String(u), MessageBody: aws.String(full + "a")})
	require.Error(t, err)
	resp := a.raw(t, sendTarget, []byte(`{"QueueUrl":"`+u+`","MessageBody":"`+full+`a"}`))
	assert.Equal(t, wireError{400, "com.amazon.coral.service#InvalidParameterValueException", "InvalidParameterValue;Sender", ""},
		withoutMessage(readWireError(t, resp)))
	a.owner.mu.Lock()
	assert.Len(t, a.owner.sent, 1, "an oversize body is never stored")
	a.owner.mu.Unlock()

	small, sq := a.create(t, "small", map[string]string{"MaximumMessageSize": "1024"})
	require.Equal(t, http.StatusOK, a.raw(t, sendTarget, []byte(`{"QueueUrl":"`+small+`","MessageBody":"`+strings.Repeat("b", 1024)+`"}`)).StatusCode)
	assert.Len(t, a.stored(t, sq, 1).Body, 1024)
	assert.Equal(t, http.StatusBadRequest, a.raw(t, sendTarget, []byte(`{"QueueUrl":"`+small+`","MessageBody":"`+strings.Repeat("b", 1025)+`"}`)).StatusCode)
}

func withoutMessage(w wireError) wireError {
	w.message = ""
	return w
}

func TestSQSSendMessage_Delay(t *testing.T) {
	a := newSQSSendApp(t, allowAllECRDocs(), nil)
	u, q := a.create(t, "delayed", map[string]string{"DelaySeconds": "30"})
	span := func(m message.Message) int64 {
		enq, err := strconv.ParseInt(m.Meta[message.MetaEnq], 10, 64)
		require.NoError(t, err)
		vis, err := strconv.ParseInt(m.Meta[message.MetaVis], 10, 64)
		require.NoError(t, err)
		return vis - enq
	}

	_, err := a.client().SendMessage(context.Background(), &sqs.SendMessageInput{QueueUrl: aws.String(u), MessageBody: aws.String("a")})
	require.NoError(t, err)
	assert.Equal(t, int64(30_000), span(a.stored(t, q, 0)), "the queue's delay applies by default")

	_, err = a.client().SendMessage(context.Background(), &sqs.SendMessageInput{QueueUrl: aws.String(u), MessageBody: aws.String("b"), DelaySeconds: 0})
	require.NoError(t, err)
	assert.Equal(t, int64(30_000), span(a.stored(t, q, 1)), "the SDK omits a zero delay")

	require.Equal(t, http.StatusOK, a.raw(t, sendTarget, sendBody(u, `,"DelaySeconds":0`)).StatusCode)
	assert.Equal(t, int64(0), span(a.stored(t, q, 2)), "an explicit zero overrides the queue")

	_, err = a.client().SendMessage(context.Background(), &sqs.SendMessageInput{QueueUrl: aws.String(u), MessageBody: aws.String("c"), DelaySeconds: 900})
	require.NoError(t, err)
	assert.Equal(t, int64(900_000), span(a.stored(t, q, 3)))
}

func TestSQSSendMessage_RequestValidation(t *testing.T) {
	a := newSQSSendApp(t, allowAllECRDocs(), nil)
	u, _ := a.create(t, "orders", nil)
	unsupported := wireError{400, "com.amazonaws.sqs#UnsupportedOperation", "AWS.SimpleQueueService.UnsupportedOperation;Sender", ""}
	invalid := wireError{400, "com.amazon.coral.service#InvalidParameterValueException", "InvalidParameterValue;Sender", ""}
	missingParam := wireError{400, "com.amazon.coral.service#MissingRequiredParameterException", "MissingParameter;Sender", ""}
	for name, tc := range map[string]struct {
		body []byte
		want wireError
	}{
		"Number attributes": {sendBody(u, `,"MessageAttributes":{"k":{"DataType":"Number","StringValue":"1"}}`), unsupported},
		"system attributes": {sendBody(u, `,"MessageSystemAttributes":{"AWSTraceHeader":{"DataType":"String","StringValue":"v"}}`), unsupported},
		"group id":          {sendBody(u, `,"MessageGroupId":"g"`), unsupported},
		"empty group id":    {sendBody(u, `,"MessageGroupId":""`), unsupported},
		"deduplication id":  {sendBody(u, `,"MessageDeduplicationId":"d"`), unsupported},
		"empty dedup id":    {sendBody(u, `,"MessageDeduplicationId":""`), unsupported},
		"null body":         {[]byte(`{"QueueUrl":"` + u + `","MessageBody":null}`), missingParam},
		"delay above 900":   {sendBody(u, `,"DelaySeconds":901`), invalid},
		"negative delay":    {sendBody(u, `,"DelaySeconds":-1`), invalid},
		"no body":           {[]byte(`{"QueueUrl":"` + u + `"}`), missingParam},
		"empty body":        {[]byte(`{"QueueUrl":"` + u + `","MessageBody":""}`), missingParam},
		"no queue url":      {[]byte(`{"MessageBody":"hello"}`), missingParam},
		"numeric body": {[]byte(`{"QueueUrl":"` + u + `","MessageBody":123}`),
			wireError{400, "com.amazon.coral.service#SerializationException", "MalformedInput;Sender", ""}},
		"nul character": {[]byte(`{"QueueUrl":"` + u + `","MessageBody":"a\u0000b"}`),
			wireError{400, "com.amazonaws.sqs#InvalidMessageContents", "InvalidMessageContents;Sender", ""}},
		"trailing input": {append(sendBody(u, ""), '}'),
			wireError{400, "com.amazon.coral.service#SerializationException", "MalformedInput;Sender", ""}},
		"wrong host": {sendBody(strings.Replace(u, "127.0.0.1", "localhost", 1), ""),
			wireError{404, "com.amazonaws.sqs#InvalidAddress", "InvalidAddress;Sender", ""}},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, withoutMessage(readWireError(t, a.raw(t, sendTarget, tc.body))))
		})
	}
	assert.Equal(t, 0, a.calls(), "invalid requests never reach the message owner")
}

func TestSQSSendMessage_MissingAndDeletingQueue(t *testing.T) {
	a := newSQSSendApp(t, allowAllECRDocs(), nil)
	u, _ := a.create(t, "doomed", nil)
	notExist := wireError{400, "com.amazonaws.sqs#QueueDoesNotExist", "AWS.SimpleQueueService.NonExistentQueue;Sender", ""}

	assert.Equal(t, notExist, withoutMessage(readWireError(t, a.raw(t, sendTarget, sendBody(a.url+"/"+ecrTestAccount+"/absent", "")))))
	_, err := a.store.MarkDeleting(context.Background(), ecrTestAccount, testRegion, "doomed")
	require.NoError(t, err)
	assert.Equal(t, notExist, withoutMessage(readWireError(t, a.raw(t, sendTarget, sendBody(u, "")))))

	_, err = a.client().SendMessage(context.Background(), &sqs.SendMessageInput{QueueUrl: aws.String(u), MessageBody: aws.String("x")})
	var nq *sqstypes.QueueDoesNotExist
	assert.ErrorAs(t, err, &nq)
	assert.Empty(t, a.owner.sent)
}

func TestSQSSendMessage_Authorization(t *testing.T) {
	queueARN := "arn:aws:sqs:" + testRegion + ":" + ecrTestAccount + ":orders"
	for name, tc := range map[string]struct {
		docs []iamdomain.PolicyDocument
		ok   bool
	}{
		"other action": {sqsPolicy([]string{"sqs:CreateQueue", "sqs:GetQueueUrl", "sqs:ReceiveMessage"}, "*"), false},
		"other queue":  {append(sqsPolicy([]string{"sqs:CreateQueue"}, "*"), sqsPolicy([]string{"sqs:SendMessage"}, queueARN+"-other")...), false},
		"this queue":   {append(sqsPolicy([]string{"sqs:CreateQueue"}, "*"), sqsPolicy([]string{"sqs:SendMessage"}, queueARN)...), true},
		"explicit deny": {append(sqsPolicy([]string{"sqs:*"}, "*"), iamdomain.PolicyDocument{Version: "2012-10-17", Statement: []iamdomain.Statement{
			{Effect: "Deny", Action: iamdomain.StringOrArr{"sqs:SendMessage"}, Resource: iamdomain.StringOrArr{queueARN + "*"}},
		}}), false},
	} {
		t.Run(name, func(t *testing.T) {
			a := newSQSSendApp(t, tc.docs, nil)
			u, _ := a.create(t, "orders", nil)
			resp := a.raw(t, sendTarget, sendBody(u, ""))
			if tc.ok {
				assert.Equal(t, http.StatusOK, resp.StatusCode)
				assert.Equal(t, 1, a.calls())
				return
			}
			assert.Equal(t, wireError{403, "com.amazon.coral.service#AccessDeniedException", "AccessDenied;Sender", ""}, withoutMessage(readWireError(t, resp)))
			absent := a.raw(t, sendTarget, sendBody(a.url+"/"+ecrTestAccount+"/orders-absent", ""))
			assert.Equal(t, "com.amazon.coral.service#AccessDeniedException", readWireError(t, absent).typ, "denial does not reveal whether a queue exists")
			assert.Equal(t, 0, a.calls())
		})
	}
}

// faultStore fails appends; landed makes the append apply before the error,
// as a lost acknowledgement does.
type faultStore struct {
	message.Store

	landed bool
}

var errAppendLost = errors.New("nats: timeout")

func (f faultStore) Append(ctx context.Context, ns message.Namespace, in message.AppendInput) (message.AppendResult, error) {
	if f.landed {
		if _, err := f.Store.Append(ctx, ns, in); err != nil {
			return message.AppendResult{}, err
		}
	}
	return message.AppendResult{}, errAppendLost
}

func TestSQSSendMessage_StoreFailureIsNeverSuccess(t *testing.T) {
	for name, landed := range map[string]bool{"append failed": false, "reply lost after append": true} {
		t.Run(name, func(t *testing.T) {
			a := newSQSSendApp(t, allowAllECRDocs(), func(s message.Store) message.Store { return faultStore{Store: s, landed: landed} })
			u, q := a.create(t, "orders", nil)
			resp := a.raw(t, sendTarget, sendBody(u, ""))
			assert.Equal(t, wireError{503, "com.amazon.coral.service#ServiceUnavailableException", "ServiceUnavailable;Receiver", ""}, withoutMessage(readWireError(t, resp)))
			assert.Empty(t, a.owner.sent)
			if landed {
				ok, err := hasMessage(a.msgs, q)
				require.NoError(t, err)
				assert.True(t, ok, "the lost-reply append did land, so a client retry may duplicate it")
			}
		})
	}
}

func hasMessage(s message.Store, q queue.Record) (bool, error) {
	_, err := s.Get(context.Background(), q.Namespace(), message.Position(1))
	if errors.Is(err, message.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// No body or credential reaches a log line or an error, on success or failure.
func TestSQSSendMessage_LogsCarryNoBodyOrCredential(t *testing.T) {
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const marker = "zq-body-marker-71c3"
	a := newSQSSendApp(t, allowAllECRDocs(), func(s message.Store) message.Store { return faultStore{Store: s} })
	u, _ := a.create(t, "orders", nil)
	var errText bytes.Buffer
	for _, body := range [][]byte{
		[]byte(`{"QueueUrl":"` + u + `","MessageBody":"` + marker + `"}`),
		[]byte(`{"QueueUrl":"` + u + `","MessageBody":"` + marker + `\u0000"}`),
		[]byte(`{"QueueUrl":"` + u + `","MessageBody":"` + marker + `","MessageGroupId":"g"}`),
		[]byte(`{"QueueUrl":"` + u + `","MessageBody":"` + marker + strings.Repeat("x", message.MaxMessageSize) + `"}`),
	} {
		resp := a.raw(t, sendTarget, body)
		errText.WriteString(readWireError(t, resp).message)
	}
	require.Positive(t, buf.Len(), "the failure paths must have logged something to inspect")
	for _, secret := range []string{marker, testSecretKey} {
		assert.NotContains(t, buf.String(), secret)
		assert.NotContains(t, errText.String(), secret)
	}
}

// encoding/json would store U+FFFD for each of these; the send must fail
// with nothing appended instead.
func TestSQSSendMessage_MalformedBodyEncodingRefused(t *testing.T) {
	a := newSQSSendApp(t, allowAllECRDocs(), nil)
	u, _ := a.create(t, "orders", nil)
	invalid := wireError{400, "com.amazonaws.sqs#InvalidMessageContents", "InvalidMessageContents;Sender", ""}
	for name, lit := range map[string]string{
		"malformed utf-8":           "a\xffb",
		"truncated utf-8":           "a\xe2\x82",
		"lone high surrogate":       `\ud800`,
		"lone low surrogate":        `\udc00`,
		"uppercase lone high":       `\uD800`,
		"high then text":            `\ud800x`,
		"high then lookalike low":   `\ud800xudc00`,
		"high then non-low escape":  `\ud800\u0041`,
		"high then high":            `\ud800\ud800`,
		"reversed pair":             `\ude00\ud83d`,
		"high surrogate at the end": `ab\ud83d`,
	} {
		t.Run(name, func(t *testing.T) {
			body := []byte(`{"QueueUrl":"` + u + `","MessageBody":"`)
			body = append(body, lit...)
			body = append(body, `"}`...)
			assert.Equal(t, invalid, withoutMessage(readWireError(t, a.raw(t, sendTarget, body))))
		})
	}
	assert.Equal(t, 0, a.calls())
}

func TestSQSSendMessage_WellFormedBodiesStoredExactly(t *testing.T) {
	a := newSQSSendApp(t, allowAllECRDocs(), nil)
	u, q := a.create(t, "orders", nil)
	cases := []struct {
		name, wire string
		want       []byte
	}{
		{"escaped surrogate pair", `\ud83d\ude00`, []byte("\U0001F600")},
		{"uppercase surrogate pair", `\uD83D\uDE00`, []byte("\U0001F600")},
		{"raw supplementary character", "\U0001F600", []byte("\U0001F600")},
		{"escaped replacement character", `\ufffd`, []byte("\uFFFD")},
		{"raw replacement character", "\uFFFD", []byte("\uFFFD")},
		{"escaped backslash before u", `\\ud800`, []byte(`\ud800`)},
		{"escaped tab and newline", `a\tb\nc`, []byte("a\tb\nc")},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := a.raw(t, sendTarget, []byte(`{"QueueUrl":"`+u+`","MessageBody":"`+tc.wire+`"}`))
			require.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Equal(t, tc.want, a.stored(t, q, i).Body)
		})
	}
}
