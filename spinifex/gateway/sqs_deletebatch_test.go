// Drives DeleteMessageBatch through the signed gateway and the SDK against
// the real queue and message owners.

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	sqsawsapi "github.com/mulgadc/spinifex/spinifex/domains/sqs/awsapi"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const deleteBatchTarget = "AmazonSQS.DeleteMessageBatch"

type batchEntry struct {
	ID            string `json:"Id"`
	ReceiptHandle string `json:"ReceiptHandle"`
}

func deleteBatchBody(t *testing.T, u string, entries ...batchEntry) []byte {
	t.Helper()
	if entries == nil {
		entries = []batchEntry{}
	}
	b, err := json.Marshal(map[string]any{"QueueUrl": u, "Entries": entries})
	require.NoError(t, err)
	return b
}

type wireBatchResult struct {
	Successful []struct {
		Id string `json:"Id"`
	} `json:"Successful"`
	Failed []struct {
		Id          string `json:"Id"`
		SenderFault bool   `json:"SenderFault"`
		Code        string `json:"Code"`
		Message     string `json:"Message"`
	} `json:"Failed"`
}

// readBatch decodes a 200 batch reply and returns its raw bytes alongside.
func readBatch(t *testing.T, resp *http.Response) (wireBatchResult, string) {
	t.Helper()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var out wireBatchResult
	require.NoError(t, json.Unmarshal(b, &out), string(b))
	return out, string(b)
}

func (r wireBatchResult) successIDs() []string {
	ids := make([]string, 0, len(r.Successful))
	for _, s := range r.Successful {
		ids = append(ids, s.Id)
	}
	sort.Strings(ids)
	return ids
}

// receiveAll receives n messages from u, across calls if a scan returns fewer.
func (a sqsRecvApp) receiveAll(t *testing.T, u string, n int, vis int32) map[string]string {
	t.Helper()
	handles := map[string]string{}
	require.Eventually(t, func() bool {
		for _, m := range a.receive(t, &sqs.ReceiveMessageInput{QueueUrl: aws.String(u), MaxNumberOfMessages: 10, VisibilityTimeout: vis}) {
			handles[aws.ToString(m.Body)] = aws.ToString(m.ReceiptHandle)
		}
		return len(handles) == n
	}, 5*time.Second, 50*time.Millisecond)
	return handles
}

// tombstoned reports whether the message handle was issued for is deleted;
// queues may share a shard, so its position comes from the handle.
func (a sqsRecvApp) tombstoned(t *testing.T, name, handle string) bool {
	t.Helper()
	q, err := a.store.Resolve(context.Background(), ecrTestAccount, testRegion, name)
	require.NoError(t, err)
	return a.tombstonedIn(t, q.Namespace(), handle)
}

func (a sqsRecvApp) tombstonedIn(t *testing.T, ns message.Namespace, handle string) bool {
	t.Helper()
	rc, err := gwReceiptKeys.Decode(handle, ns.Queue)
	require.NoError(t, err)
	st, err := a.msgs.GetState(context.Background(), ns, rc.Position)
	require.NoError(t, err)
	return strings.Contains(string(st.Value), `"s":"deleted"`)
}

func TestSQSDeleteMessageBatch_SDKDeletesEveryEntry(t *testing.T) {
	a := newSQSRecvApp(t, allowAllECRDocs(), nil)
	u := a.create(t, "orders", nil)
	for i := 1; i <= 3; i++ {
		a.send(t, u, "m"+strconv.Itoa(i))
	}
	handles := a.receiveAll(t, u, 3, 0)

	in := &sqs.DeleteMessageBatchInput{QueueUrl: aws.String(u)}
	for i := 1; i <= 3; i++ {
		in.Entries = append(in.Entries, sqstypes.DeleteMessageBatchRequestEntry{Id: aws.String("e" + strconv.Itoa(i)), ReceiptHandle: aws.String(handles["m"+strconv.Itoa(i)])})
	}
	out, err := a.client().DeleteMessageBatch(context.Background(), in)
	require.NoError(t, err)
	assert.Empty(t, out.Failed)
	var ids []string
	for _, s := range out.Successful {
		ids = append(ids, aws.ToString(s.Id))
	}
	assert.ElementsMatch(t, []string{"e1", "e2", "e3"}, ids)
	for body, h := range handles {
		assert.True(t, a.tombstoned(t, "orders", h), "%s deleted", body)
	}
	_, d := a.owner.counts()
	assert.Equal(t, 3, d)
}

// Each entry is reported under its own Id; a stale handle succeeds without
// deleting, and invalid or other-queue handles fail only their own entry.
func TestSQSDeleteMessageBatch_PartialFailure(t *testing.T) {
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	a := newSQSRecvApp(t, allowAllECRDocs(), nil)
	u := a.create(t, "orders", nil)
	other := a.create(t, "other", nil)
	const keep, drop, elsewhere = "zq-batch-keep-71c2", "zq-batch-drop-71c2", "zq-batch-else-71c2"
	a.send(t, u, keep)
	stale := a.receiveAll(t, u, 1, 1)[keep]
	require.Eventually(t, func() bool {
		return len(a.receive(t, &sqs.ReceiveMessageInput{QueueUrl: aws.String(u), VisibilityTimeout: 300})) == 1
	}, 5*time.Second, 100*time.Millisecond, "re-received once visibility lapses")
	a.send(t, u, drop)
	dropHandle := a.receiveAll(t, u, 1, 300)[drop]
	a.send(t, other, elsewhere)
	foreign := a.receiveAll(t, other, 1, 300)[elsewhere]

	tampered := []byte(dropHandle)
	mid := len(tampered) / 2
	tampered[mid] = map[bool]byte{true: 'B', false: 'A'}[tampered[mid] == 'A']
	resp := a.raw(t, deleteBatchTarget, deleteBatchBody(t, u,
		batchEntry{"stale", stale},
		batchEntry{"tampered", string(tampered)},
		batchEntry{"garbage", "not-a-handle"},
		batchEntry{"other-queue", foreign},
	))
	out, raw := readBatch(t, resp)
	assert.Equal(t, []string{"stale"}, out.successIDs())
	require.Len(t, out.Failed, 3)
	for i, want := range []string{"tampered", "garbage", "other-queue"} {
		f := out.Failed[i]
		assert.Equal(t, want, f.Id)
		assert.True(t, f.SenderFault)
		assert.Equal(t, "ReceiptHandleIsInvalid", f.Code)
	}
	assert.False(t, a.tombstoned(t, "orders", stale), "a stale handle deletes nothing")
	assert.False(t, a.tombstoned(t, "orders", dropHandle), "a tampered handle deletes nothing")
	assert.False(t, a.tombstoned(t, "other", foreign), "another queue's handle deletes nothing")

	require.Positive(t, buf.Len(), "the failed entries must have logged something to inspect")
	for _, secret := range []string{stale, string(tampered), foreign, dropHandle, keep, drop, elsewhere, testSecretKey} {
		assert.NotContains(t, buf.String(), secret)
		assert.NotContains(t, raw, secret)
	}
}

// splitDeleter sends one handle's delete to a faulty owner and the rest to
// the real one.
type splitDeleter struct {
	ok, faulty sqsawsapi.Deletes
	handle     string
}

func (s splitDeleter) Delete(ctx context.Context, in message.DeleteInput) error {
	if in.ReceiptHandle == s.handle {
		return s.faulty.Delete(ctx, in)
	}
	return s.ok.Delete(ctx, in)
}

func TestSQSDeleteMessageBatch_UncertainEntryIsNeverSuccess(t *testing.T) {
	for name, landed := range map[string]bool{"tombstone failed": false, "tombstone reply lost after landing": true} {
		t.Run(name, func(t *testing.T) {
			a := newSQSRecvApp(t, allowAllECRDocs(), nil)
			u := a.create(t, "orders", nil)
			a.send(t, u, "first")
			a.send(t, u, "second")
			handles := a.receiveAll(t, u, 2, 300)
			a.owner.mu.Lock()
			a.owner.del = splitDeleter{
				ok:     a.owner.del,
				faulty: &message.Deleter{Store: recvFaultStore{Store: a.msgs, landed: landed}, Queues: a.store, Keys: gwReceiptKeys},
				handle: handles["second"],
			}
			a.owner.mu.Unlock()

			out, err := a.client().DeleteMessageBatch(context.Background(), &sqs.DeleteMessageBatchInput{
				QueueUrl: aws.String(u),
				Entries: []sqstypes.DeleteMessageBatchRequestEntry{
					{Id: aws.String("a"), ReceiptHandle: aws.String(handles["first"])},
					{Id: aws.String("b"), ReceiptHandle: aws.String(handles["second"])},
				},
			})
			require.NoError(t, err)
			assert.True(t, a.tombstoned(t, "orders", handles["first"]))
			if landed {
				// The re-read confirms the tombstone landed, so the entry is settled.
				assert.Len(t, out.Successful, 2)
				assert.Empty(t, out.Failed)
				assert.True(t, a.tombstoned(t, "orders", handles["second"]))
				return
			}
			require.Len(t, out.Successful, 1)
			assert.Equal(t, "a", aws.ToString(out.Successful[0].Id))
			require.Len(t, out.Failed, 1)
			f := out.Failed[0]
			assert.Equal(t, "b", aws.ToString(f.Id))
			assert.False(t, f.SenderFault, "an unconfirmed delete is the service's fault, so the caller retries it")
			assert.Equal(t, "ServiceUnavailable", aws.ToString(f.Code))
			assert.False(t, a.tombstoned(t, "orders", handles["second"]))
		})
	}
}

func TestSQSDeleteMessageBatch_EnvelopeRefusedBeforeAnyDelete(t *testing.T) {
	a := newSQSRecvApp(t, allowAllECRDocs(), nil)
	u := a.create(t, "orders", nil)
	eleven := make([]batchEntry, 11)
	for i := range eleven {
		eleven[i] = batchEntry{"e" + strconv.Itoa(i), "h"}
	}
	wireBatch := func(name string) wireError {
		return wireError{400, "com.amazonaws.sqs#" + name, "AWS.SimpleQueueService." + name + ";Sender", ""}
	}
	for name, tc := range map[string]struct {
		body []byte
		want wireError
	}{
		"no entries":       {deleteBatchBody(t, u), wireBatch("EmptyBatchRequest")},
		"eleven entries":   {deleteBatchBody(t, u, eleven...), wireBatch("TooManyEntriesInBatchRequest")},
		"bad id character": {deleteBatchBody(t, u, batchEntry{"ok", "h"}, batchEntry{"bad.id", "h"}), wireBatch("InvalidBatchEntryId")},
		"id too long":      {deleteBatchBody(t, u, batchEntry{strings.Repeat("a", 81), "h"}), wireBatch("InvalidBatchEntryId")},
		"empty id":         {deleteBatchBody(t, u, batchEntry{"", "h"}), wireBatch("InvalidBatchEntryId")},
		"repeated id":      {deleteBatchBody(t, u, batchEntry{"x", "h1"}, batchEntry{"x", "h2"}), wireBatch("BatchEntryIdsNotDistinct")},
		"missing handle":   {deleteBatchBody(t, u, batchEntry{"x", "h"}, batchEntry{"y", ""}), wireMissingParam},
		"missing entries":  {[]byte(`{"QueueUrl":"` + u + `"}`), wireMissingParam},
		"null entries":     {[]byte(`{"QueueUrl":"` + u + `","Entries":null}`), wireMissingParam},
		"missing url":      {[]byte(`{"Entries":[{"Id":"x","ReceiptHandle":"h"}]}`), wireMissingParam},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, withoutMessage(readWireError(t, a.raw(t, deleteBatchTarget, tc.body))))
		})
	}
	trailing := append(deleteBatchBody(t, u, batchEntry{"x", "h"}), []byte(`{}`)...)
	assert.Equal(t, http.StatusBadRequest, a.raw(t, deleteBatchTarget, trailing).StatusCode)
	assert.Equal(t, wireBatch("TooManyEntriesInBatchRequest"), withoutMessage(readWireError(t, a.raw(t, deleteBatchTarget,
		deleteBatchBody(t, a.url+"/999999999999/orders", eleven...)))), "the batch is checked before the queue address")
	_, d := a.owner.counts()
	assert.Zero(t, d, "no entry reaches the owner")
	maxIDs := make([]batchEntry, 10)
	for i := range maxIDs {
		maxIDs[i] = batchEntry{strings.Repeat("Az_-9", 16)[:79] + strconv.Itoa(i), "h"}
	}
	out, _ := readBatch(t, a.raw(t, deleteBatchTarget, deleteBatchBody(t, u, maxIDs...)))
	assert.Len(t, out.Failed, 10, "ten 80-character ids are accepted and each forged handle fails alone")
}

func TestSQSDeleteMessageBatch_Authorization(t *testing.T) {
	queueARN := "arn:aws:sqs:" + testRegion + ":" + ecrTestAccount + ":orders"
	setup := sqsPolicy([]string{"sqs:CreateQueue", "sqs:SendMessage", "sqs:ReceiveMessage"}, "*")
	for name, tc := range map[string]struct {
		docs []iamdomain.PolicyDocument
		ok   bool
	}{
		"other actions": {append(setup, sqsPolicy([]string{"sqs:ChangeMessageVisibility", "sqs:DeleteMessageBatch"}, "*")...), false},
		"other queue":   {append(setup, sqsPolicy([]string{"sqs:DeleteMessage"}, queueARN+"-other")...), false},
		"delete here":   {append(setup, sqsPolicy([]string{"sqs:DeleteMessage"}, queueARN)...), true},
		"explicit deny": {append(sqsPolicy([]string{"sqs:*"}, "*"), iamdomain.PolicyDocument{Version: "2012-10-17", Statement: []iamdomain.Statement{
			{Effect: "Deny", Action: iamdomain.StringOrArr{"sqs:DeleteMessage"}, Resource: iamdomain.StringOrArr{queueARN + "*"}},
		}}), false},
	} {
		t.Run(name, func(t *testing.T) {
			a := newSQSRecvApp(t, tc.docs, nil)
			u := a.create(t, "orders", nil)
			a.send(t, u, "hello")
			h := a.receiveAll(t, u, 1, 300)["hello"]
			absent := a.url + "/" + ecrTestAccount + "/orders-absent"

			resp := a.raw(t, deleteBatchTarget, deleteBatchBody(t, u, batchEntry{"x", h}))
			if tc.ok {
				out, _ := readBatch(t, resp)
				assert.Equal(t, []string{"x"}, out.successIDs())
				assert.True(t, a.tombstoned(t, "orders", h))
				return
			}
			assert.Equal(t, wireDenied, withoutMessage(readWireError(t, resp)))
			assert.Equal(t, wireDenied.typ, readWireError(t, a.raw(t, deleteBatchTarget, deleteBatchBody(t, absent, batchEntry{"x", h}))).typ,
				"denial does not reveal whether a queue exists")
			_, d := a.owner.counts()
			assert.Zero(t, d)
			assert.False(t, a.tombstoned(t, "orders", h))
		})
	}
}
