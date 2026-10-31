// Drives SendMessageBatch through the signed gateway and the SDK against the
// real queue owner and message owner, with the message store on its own
// embedded NATS server.

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/queue"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sendBatchTarget = "AmazonSQS.SendMessageBatch"

// sendBatchBody builds a request whose entries are raw JSON objects, so a test
// can supply members the SDK would never send.
func sendBatchBody(t *testing.T, u string, entries ...string) []byte {
	t.Helper()
	b, err := json.Marshal(u)
	require.NoError(t, err)
	return []byte(`{"QueueUrl":` + string(b) + `,"Entries":[` + strings.Join(entries, ",") + `]}`)
}

// sendEntry is one entry with a JSON-encoded body and optional extra members.
func sendEntry(t *testing.T, id, body, rest string) string {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	return `{"Id":"` + id + `","MessageBody":` + string(b) + rest + `}`
}

type wireSendBatch struct {
	Successful []struct {
		Id               string `json:"Id"`
		MessageId        string `json:"MessageId"`
		MD5OfMessageBody string `json:"MD5OfMessageBody"`
	} `json:"Successful"`
	Failed []struct {
		Id          string `json:"Id"`
		SenderFault bool   `json:"SenderFault"`
		Code        string `json:"Code"`
		Message     string `json:"Message"`
	} `json:"Failed"`
}

func readSendBatch(t *testing.T, resp *http.Response) (wireSendBatch, string) {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(b))
	var out wireSendBatch
	require.NoError(t, json.Unmarshal(b, &out), string(b))
	return out, string(b)
}

func (w wireSendBatch) failures() map[string]string {
	got := map[string]string{}
	for _, f := range w.Failed {
		got[f.Id] = f.Code + map[bool]string{true: ";Sender", false: ";Receiver"}[f.SenderFault]
	}
	return got
}

// storedBodies reads every message the queue holds, keyed by body.
func (a sqsSendApp) storedBodies(t *testing.T, q queue.Record) map[string]message.Message {
	t.Helper()
	got := map[string]message.Message{}
	for pos := message.Position(1); pos <= 12; pos++ {
		m, err := a.msgs.Get(context.Background(), q.Namespace(), pos)
		if errors.Is(err, message.ErrNotFound) {
			continue
		}
		require.NoError(t, err)
		got[string(m.Body)] = m
	}
	return got
}

// The SDK checks every successful entry's MD5OfMessageBody against what it
// sent, so a wrong digest fails the call.
func TestSQSSendMessageBatch_SDKStoresEveryEntry(t *testing.T) {
	a := newSQSSendApp(t, allowAllECRDocs(), nil)
	u, q := a.create(t, "orders", nil)
	in := &sqs.SendMessageBatchInput{QueueUrl: aws.String(u)}
	for i := 1; i <= 10; i++ {
		in.Entries = append(in.Entries, sqstypes.SendMessageBatchRequestEntry{Id: aws.String("e" + strconv.Itoa(i)), MessageBody: aws.String("body-" + strconv.Itoa(i))})
	}
	out, err := a.client().SendMessageBatch(context.Background(), in)
	require.NoError(t, err)
	assert.Empty(t, out.Failed)
	require.Len(t, out.Successful, 10)
	ids := map[string]bool{}
	for i, s := range out.Successful {
		assert.Equal(t, "e"+strconv.Itoa(i+1), aws.ToString(s.Id))
		assert.Equal(t, md5Hex([]byte("body-"+strconv.Itoa(i+1))), aws.ToString(s.MD5OfMessageBody))
		ids[aws.ToString(s.MessageId)] = true
	}
	assert.Len(t, ids, 10, "every message gets its own ID")

	stored := a.storedBodies(t, q)
	require.Len(t, stored, 10)
	for i, s := range out.Successful {
		m := stored["body-"+strconv.Itoa(i+1)]
		assert.Equal(t, aws.ToString(s.MessageId), m.Meta[message.MetaID])
		assert.Equal(t, aliceUserID, m.Meta[message.MetaSender])
	}
}

// An entry the owner would refuse fails alone and is never stored; an
// unsupported member is refused rather than accepted inert.
func TestSQSSendMessageBatch_PartialInvalidEntries(t *testing.T) {
	a := newSQSSendApp(t, allowAllECRDocs(), nil)
	u, q := a.create(t, "small", map[string]string{"MaximumMessageSize": "1024"})
	resp := a.raw(t, sendBatchTarget, sendBatchBody(t, u,
		sendEntry(t, "ok1", "first", ""),
		sendEntry(t, "nul", "a\x00b", ""),
		`{"Id":"surrogate","MessageBody":"a\ud800b"}`,
		sendEntry(t, "delay", "late", `,"DelaySeconds":901`),
		sendEntry(t, "attrs", "tagged", `,"MessageAttributes":{"k":{"DataType":"Number","StringValue":"1"}}`),
		sendEntry(t, "sysattrs", "traced", `,"MessageSystemAttributes":{"AWSTraceHeader":{"DataType":"String","StringValue":"v"}}`),
		sendEntry(t, "group", "grouped", `,"MessageGroupId":""`),
		sendEntry(t, "dedup", "deduped", `,"MessageDeduplicationId":"d"`),
		`{"Id":"nobody"}`,
		sendEntry(t, "big", strings.Repeat("b", 1025), ""),
	))
	out, _ := readSendBatch(t, resp)
	require.Len(t, out.Successful, 1)
	assert.Equal(t, "ok1", out.Successful[0].Id)
	assert.Equal(t, map[string]string{
		"nul":       "InvalidMessageContents;Sender",
		"surrogate": "InvalidMessageContents;Sender",
		"delay":     "InvalidParameterValue;Sender",
		"attrs":     "AWS.SimpleQueueService.UnsupportedOperation;Sender",
		"sysattrs":  "AWS.SimpleQueueService.UnsupportedOperation;Sender",
		"group":     "AWS.SimpleQueueService.UnsupportedOperation;Sender",
		"dedup":     "AWS.SimpleQueueService.UnsupportedOperation;Sender",
		"nobody":    "MissingParameter;Sender",
		"big":       "InvalidParameterValue;Sender",
	}, out.failures())
	var order []string
	for _, f := range out.Failed {
		order = append(order, f.Id)
	}
	assert.Equal(t, []string{"nul", "surrogate", "delay", "attrs", "sysattrs", "group", "dedup", "nobody", "big"}, order, "failures keep entry order")
	assert.Equal(t, 2, a.calls(), "only the valid entry and the one needing the queue's size limit reach the owner")
	stored := a.storedBodies(t, q)
	assert.Len(t, stored, 1)
	assert.Contains(t, stored, "first")

	full := strings.Repeat("c", 1024)
	out, _ = readSendBatch(t, a.raw(t, sendBatchTarget, sendBatchBody(t, u, sendEntry(t, "full", full, ""))))
	assert.Len(t, out.Successful, 1, "a body at the queue's limit is accepted")
}

func TestSQSSendMessageBatch_RefusedBeforeAnyAppend(t *testing.T) {
	a := newSQSSendApp(t, allowAllECRDocs(), nil)
	u, q := a.create(t, "orders", nil)
	batch := func(name string) wireError {
		return wireError{400, "com.amazonaws.sqs#" + name, "AWS.SimpleQueueService." + name + ";Sender", ""}
	}
	eleven := make([]string, 11)
	for i := range eleven {
		eleven[i] = sendEntry(t, "e"+strconv.Itoa(i), "x", "")
	}
	half := strings.Repeat("h", maxBatchHalf)
	malformed := wireError{400, "com.amazon.coral.service#SerializationException", "MalformedInput;Sender", ""}
	for name, tc := range map[string]struct {
		body []byte
		want wireError
	}{
		"no entries":      {sendBatchBody(t, u), batch("EmptyBatchRequest")},
		"eleven entries":  {sendBatchBody(t, u, eleven...), batch("TooManyEntriesInBatchRequest")},
		"bad id":          {sendBatchBody(t, u, sendEntry(t, "a.b", "x", "")), batch("InvalidBatchEntryId")},
		"id too long":     {sendBatchBody(t, u, sendEntry(t, strings.Repeat("a", 81), "x", "")), batch("InvalidBatchEntryId")},
		"no id":           {sendBatchBody(t, u, `{"MessageBody":"x"}`), batch("InvalidBatchEntryId")},
		"repeated id":     {sendBatchBody(t, u, sendEntry(t, "x", "1", ""), sendEntry(t, "x", "2", "")), batch("BatchEntryIdsNotDistinct")},
		"over total size": {sendBatchBody(t, u, sendEntry(t, "a", half, ""), sendEntry(t, "b", half+"h", "")), batch("BatchRequestTooLong")},
		"invalid entry still counts": {sendBatchBody(t, u, sendEntry(t, "a", half, ""), sendEntry(t, "b", half+"h", `,"DelaySeconds":901`)),
			batch("BatchRequestTooLong")},
		"missing entries": {[]byte(`{"QueueUrl":"` + u + `"}`), wireMissingParam},
		"missing url":     {[]byte(`{"Entries":[{"Id":"a","MessageBody":"x"}]}`), wireMissingParam},
		"numeric body":    {sendBatchBody(t, u, `{"Id":"a","MessageBody":1}`), malformed},
		"trailing input":  {append(sendBatchBody(t, u, sendEntry(t, "a", "x", "")), '{', '}'), malformed},
		"foreign account": {sendBatchBody(t, a.url+"/999988887777/orders", sendEntry(t, "a", "x", "")),
			wireError{404, "com.amazonaws.sqs#InvalidAddress", "InvalidAddress;Sender", ""}},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, withoutMessage(readWireError(t, a.raw(t, sendBatchTarget, tc.body))))
		})
	}
	assert.Equal(t, 0, a.calls(), "no entry of a refused batch reaches the owner")
	assert.Empty(t, a.storedBodies(t, q))

	out, _ := readSendBatch(t, a.raw(t, sendBatchTarget, sendBatchBody(t, u, sendEntry(t, "a", half, ""), sendEntry(t, "b", half, ""))))
	assert.Len(t, out.Successful, 2, "bodies totalling exactly the limit are accepted")
}

// maxBatchHalf is half the batch payload limit.
const maxBatchHalf = message.MaxMessageSize / 2

func TestSQSSendMessageBatch_Delay(t *testing.T) {
	a := newSQSSendApp(t, allowAllECRDocs(), nil)
	u, q := a.create(t, "delayed", map[string]string{"DelaySeconds": "30"})
	out, _ := readSendBatch(t, a.raw(t, sendBatchTarget, sendBatchBody(t, u,
		sendEntry(t, "default", "d", ""),
		sendEntry(t, "zero", "z", `,"DelaySeconds":0`),
		sendEntry(t, "max", "m", `,"DelaySeconds":900`),
	)))
	require.Len(t, out.Successful, 3)
	span := func(m message.Message) int64 {
		enq, err := strconv.ParseInt(m.Meta[message.MetaEnq], 10, 64)
		require.NoError(t, err)
		vis, err := strconv.ParseInt(m.Meta[message.MetaVis], 10, 64)
		require.NoError(t, err)
		return vis - enq
	}
	stored := a.storedBodies(t, q)
	assert.Equal(t, int64(30_000), span(stored["d"]), "the queue's delay applies by default")
	assert.Equal(t, int64(0), span(stored["z"]), "an explicit zero overrides the queue")
	assert.Equal(t, int64(900_000), span(stored["m"]))
}

func TestSQSSendMessageBatch_Authorization(t *testing.T) {
	queueARN := "arn:aws:sqs:" + testRegion + ":" + ecrTestAccount + ":orders"
	for name, tc := range map[string]struct {
		docs []iamdomain.PolicyDocument
		ok   bool
	}{
		"other actions": {sqsPolicy([]string{"sqs:CreateQueue", "sqs:ReceiveMessage", "sqs:SendMessageBatch"}, "*"), false},
		"other queue":   {append(sqsPolicy([]string{"sqs:CreateQueue"}, "*"), sqsPolicy([]string{"sqs:SendMessage"}, queueARN+"-other")...), false},
		"this queue":    {append(sqsPolicy([]string{"sqs:CreateQueue"}, "*"), sqsPolicy([]string{"sqs:SendMessage"}, queueARN)...), true},
		"explicit deny": {append(sqsPolicy([]string{"sqs:*"}, "*"), iamdomain.PolicyDocument{Version: "2012-10-17", Statement: []iamdomain.Statement{
			{Effect: "Deny", Action: iamdomain.StringOrArr{"sqs:SendMessage"}, Resource: iamdomain.StringOrArr{queueARN + "*"}},
		}}), false},
	} {
		t.Run(name, func(t *testing.T) {
			a := newSQSSendApp(t, tc.docs, nil)
			u, q := a.create(t, "orders", nil)
			resp := a.raw(t, sendBatchTarget, sendBatchBody(t, u, sendEntry(t, "a", "x", ""), sendEntry(t, "b", "y", "")))
			if tc.ok {
				out, _ := readSendBatch(t, resp)
				assert.Len(t, out.Successful, 2)
				assert.Equal(t, 2, a.calls())
				return
			}
			assert.Equal(t, wireDenied, withoutMessage(readWireError(t, resp)))
			absent := a.raw(t, sendBatchTarget, sendBatchBody(t, a.url+"/"+ecrTestAccount+"/orders-absent", sendEntry(t, "a", "x", "")))
			assert.Equal(t, wireDenied.typ, readWireError(t, absent).typ, "denial does not reveal whether a queue exists")
			assert.Equal(t, 0, a.calls())
			assert.Empty(t, a.storedBodies(t, q))
		})
	}
}

// bodyFaultStore fails the append of one body; landed makes it apply before
// the error, as a lost acknowledgement does.
type bodyFaultStore struct {
	message.Store

	body   string
	landed bool
}

func (f bodyFaultStore) Append(ctx context.Context, ns message.Namespace, in message.AppendInput) (message.AppendResult, error) {
	if string(in.Body) != f.body {
		return f.Store.Append(ctx, ns, in)
	}
	if f.landed {
		if _, err := f.Store.Append(ctx, ns, in); err != nil {
			return message.AppendResult{}, err
		}
	}
	return message.AppendResult{}, errAppendLost
}

func TestSQSSendMessageBatch_UncertainAppendIsRetryableFailure(t *testing.T) {
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const lost = "zq-sendbatch-lost-4d2e"
	for name, landed := range map[string]bool{"append failed": false, "applied but reply lost": true} {
		t.Run(name, func(t *testing.T) {
			a := newSQSSendApp(t, allowAllECRDocs(), func(s message.Store) message.Store { return bodyFaultStore{Store: s, body: lost, landed: landed} })
			u, q := a.create(t, "orders", nil)
			out, err := a.client().SendMessageBatch(context.Background(), &sqs.SendMessageBatchInput{
				QueueUrl: aws.String(u),
				Entries: []sqstypes.SendMessageBatchRequestEntry{
					{Id: aws.String("a"), MessageBody: aws.String("before")},
					{Id: aws.String("b"), MessageBody: aws.String(lost)},
					{Id: aws.String("c"), MessageBody: aws.String("after")},
				},
			})
			require.NoError(t, err)
			require.Len(t, out.Successful, 2)
			assert.Equal(t, "a", aws.ToString(out.Successful[0].Id))
			assert.Equal(t, "c", aws.ToString(out.Successful[1].Id))
			require.Len(t, out.Failed, 1)
			f := out.Failed[0]
			assert.Equal(t, "b", aws.ToString(f.Id))
			assert.False(t, f.SenderFault, "an unconfirmed append is retryable, not a client fault")
			assert.Equal(t, "ServiceUnavailable", aws.ToString(f.Code))

			stored := a.storedBodies(t, q)
			assert.Contains(t, stored, "before")
			assert.Contains(t, stored, "after")
			_, applied := stored[lost]
			assert.Equal(t, landed, applied, "a lost reply can still have stored the message, so a retry may duplicate it")
		})
	}
	require.Positive(t, buf.Len())
	for _, secret := range []string{lost, "before", "after", testSecretKey} {
		assert.NotContains(t, buf.String(), secret)
	}
}

// No body or credential reaches a log line or a failed entry's message.
func TestSQSSendMessageBatch_FailuresCarryNoBodyOrCredential(t *testing.T) {
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const marker = "zq-sendbatch-marker-91af"
	a := newSQSSendApp(t, allowAllECRDocs(), nil)
	u, _ := a.create(t, "small", map[string]string{"MaximumMessageSize": "1024"})
	_, raw := readSendBatch(t, a.raw(t, sendBatchTarget, sendBatchBody(t, u,
		sendEntry(t, "nul", marker+"\x00", ""),
		sendEntry(t, "big", marker+strings.Repeat("b", 1024), ""),
		sendEntry(t, "attrs", marker, `,"MessageAttributes":{"`+marker+`":{"DataType":"String","StringValue":"`+marker+`"}}`),
	)))
	require.Positive(t, buf.Len())
	for _, secret := range []string{marker, testSecretKey} {
		assert.NotContains(t, buf.String(), secret)
		assert.NotContains(t, raw, secret)
	}
}
