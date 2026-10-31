// Drives ChangeMessageVisibilityBatch through the signed gateway and the SDK
// against the real queue and message owners.

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
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

const changeBatchTarget = "AmazonSQS.ChangeMessageVisibilityBatch"

type visEntry struct {
	ID                string `json:"Id"`
	ReceiptHandle     string `json:"ReceiptHandle"`
	VisibilityTimeout int    `json:"VisibilityTimeout"`
}

func changeBatchBody(t *testing.T, u string, entries ...visEntry) []byte {
	t.Helper()
	if entries == nil {
		entries = []visEntry{}
	}
	b, err := json.Marshal(map[string]any{"QueueUrl": u, "Entries": entries})
	require.NoError(t, err)
	return b
}

func readChangeBatch(t *testing.T, resp *http.Response) (wireBatchResult, string) {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(b))
	var out wireBatchResult
	require.NoError(t, json.Unmarshal(b, &out), string(b))
	return out, string(b)
}

func (r wireBatchResult) codes() map[string]string {
	got := map[string]string{}
	for _, f := range r.Failed {
		got[f.Id] = f.Code + map[bool]string{true: ";Sender", false: ";Receiver"}[f.SenderFault]
	}
	return got
}

// visibleNow receives with a long timeout and returns the bodies that were
// visible, hiding them again for the rest of the test.
func (a sqsRecvApp) visibleNow(t *testing.T, u string) []string {
	t.Helper()
	var bodies []string
	for _, m := range a.receive(t, &sqs.ReceiveMessageInput{QueueUrl: aws.String(u), MaxNumberOfMessages: 10, VisibilityTimeout: 300}) {
		bodies = append(bodies, aws.ToString(m.Body))
	}
	return bodies
}

func TestSQSChangeMessageVisibilityBatch_SDKChangesEveryEntry(t *testing.T) {
	a := newSQSRecvApp(t, allowAllECRDocs(), nil)
	u := a.create(t, "orders", nil)
	for i := 1; i <= 3; i++ {
		a.send(t, u, "m"+strconv.Itoa(i))
	}
	handles := a.receiveAll(t, u, 3, 300)
	require.Empty(t, a.visibleNow(t, u))

	in := &sqs.ChangeMessageVisibilityBatchInput{QueueUrl: aws.String(u)}
	for i := 1; i <= 3; i++ {
		in.Entries = append(in.Entries, sqstypes.ChangeMessageVisibilityBatchRequestEntry{
			Id: aws.String("e" + strconv.Itoa(i)), ReceiptHandle: aws.String(handles["m"+strconv.Itoa(i)]), VisibilityTimeout: 0,
		})
	}
	out, err := a.client().ChangeMessageVisibilityBatch(context.Background(), in)
	require.NoError(t, err)
	assert.Empty(t, out.Failed)
	var ids []string
	for _, s := range out.Successful {
		ids = append(ids, aws.ToString(s.Id))
	}
	assert.Equal(t, []string{"e1", "e2", "e3"}, ids)
	assert.ElementsMatch(t, []string{"m1", "m2", "m3"}, a.visibleNow(t, u), "a zero timeout makes each message visible at once")
	assert.Equal(t, 3, a.owner.changeCount())
}

// Each entry is reported under its own Id; a bad handle or timeout fails only
// its own entry and changes nothing.
func TestSQSChangeMessageVisibilityBatch_PartialFailure(t *testing.T) {
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	a := newSQSRecvApp(t, allowAllECRDocs(), nil)
	u := a.create(t, "orders", nil)
	other := a.create(t, "other", nil)
	const stale, live, held, elsewhere = "zq-vis-stale-3b7d", "zq-vis-live-3b7d", "zq-vis-held-3b7d", "zq-vis-else-3b7d"
	a.send(t, u, stale)
	staleHandle := a.receiveAll(t, u, 1, 1)[stale]
	require.Eventually(t, func() bool { return len(a.visibleNow(t, u)) == 1 }, 5*time.Second, 100*time.Millisecond, "re-received once visibility lapses")
	a.send(t, u, live)
	a.send(t, u, held)
	handles := a.receiveAll(t, u, 2, 300)
	a.send(t, other, elsewhere)
	foreign := a.receiveAll(t, other, 1, 300)[elsewhere]
	tampered := []byte(handles[held])
	mid := len(tampered) / 2
	tampered[mid] = map[bool]byte{true: 'B', false: 'A'}[tampered[mid] == 'A']

	out, raw := readChangeBatch(t, a.raw(t, changeBatchTarget, changeBatchBody(t, u,
		visEntry{"live", handles[live], 0},
		visEntry{"stale", staleHandle, 0},
		visEntry{"tampered", string(tampered), 0},
		visEntry{"garbage", "not-a-handle", 0},
		visEntry{"other-queue", foreign, 0},
		visEntry{"negative", handles[held], -1},
		visEntry{"too-long", handles[held], 43201},
		visEntry{"past-cap", handles[held], 43200},
	)))
	assert.Equal(t, []string{"live"}, out.successIDs())
	assert.Equal(t, map[string]string{
		"stale":       "AWS.SimpleQueueService.MessageNotInflight;Sender",
		"tampered":    "ReceiptHandleIsInvalid;Sender",
		"garbage":     "ReceiptHandleIsInvalid;Sender",
		"other-queue": "ReceiptHandleIsInvalid;Sender",
		"negative":    "InvalidParameterValue;Sender",
		"too-long":    "InvalidParameterValue;Sender",
		"past-cap":    "InvalidParameterValue;Sender",
	}, out.codes())
	var order []string
	for _, f := range out.Failed {
		order = append(order, f.Id)
	}
	assert.Equal(t, []string{"stale", "tampered", "garbage", "other-queue", "negative", "too-long", "past-cap"}, order)
	assert.Equal(t, 6, a.owner.changeCount(), "out-of-range timeouts never reach the owner")
	assert.Equal(t, []string{live}, a.visibleNow(t, u), "only the valid entry changed; the held message stays hidden")
	assert.Empty(t, a.visibleNow(t, other))

	require.Positive(t, buf.Len())
	for _, secret := range []string{staleHandle, handles[live], handles[held], string(tampered), foreign, stale, live, held, elsewhere, testSecretKey} {
		assert.NotContains(t, buf.String(), secret)
		assert.NotContains(t, raw, secret)
	}
}

func TestSQSChangeMessageVisibilityBatch_Authorization(t *testing.T) {
	queueARN := "arn:aws:sqs:" + testRegion + ":" + ecrTestAccount + ":orders"
	setup := sqsPolicy([]string{"sqs:CreateQueue", "sqs:SendMessage", "sqs:ReceiveMessage"}, "*")
	for name, tc := range map[string]struct {
		docs []iamdomain.PolicyDocument
		ok   bool
	}{
		"other actions": {append(setup, sqsPolicy([]string{"sqs:DeleteMessage", "sqs:ChangeMessageVisibilityBatch"}, "*")...), false},
		"other queue":   {append(setup, sqsPolicy([]string{"sqs:ChangeMessageVisibility"}, queueARN+"-other")...), false},
		"this queue":    {append(setup, sqsPolicy([]string{"sqs:ChangeMessageVisibility"}, queueARN)...), true},
		"explicit deny": {append(sqsPolicy([]string{"sqs:*"}, "*"), iamdomain.PolicyDocument{Version: "2012-10-17", Statement: []iamdomain.Statement{
			{Effect: "Deny", Action: iamdomain.StringOrArr{"sqs:ChangeMessageVisibility"}, Resource: iamdomain.StringOrArr{queueARN + "*"}},
		}}), false},
	} {
		t.Run(name, func(t *testing.T) {
			a := newSQSRecvApp(t, tc.docs, nil)
			u := a.create(t, "orders", nil)
			a.send(t, u, "hello")
			h := a.receiveAll(t, u, 1, 300)["hello"]
			resp := a.raw(t, changeBatchTarget, changeBatchBody(t, u, visEntry{"x", h, 0}))
			if tc.ok {
				out, _ := readChangeBatch(t, resp)
				assert.Equal(t, []string{"x"}, out.successIDs())
				return
			}
			assert.Equal(t, wireDenied, withoutMessage(readWireError(t, resp)))
			absent := a.raw(t, changeBatchTarget, changeBatchBody(t, a.url+"/"+ecrTestAccount+"/orders-absent", visEntry{"x", h, 0}))
			assert.Equal(t, wireDenied.typ, readWireError(t, absent).typ, "denial does not reveal whether a queue exists")
			assert.Zero(t, a.owner.changeCount())
		})
	}
}

// splitVisibility sends one handle's change to a faulty owner and the rest
// to the real one.
type splitVisibility struct {
	ok, faulty sqsawsapi.Visibility
	handle     string
}

func (s splitVisibility) ChangeVisibility(ctx context.Context, in message.ChangeVisibilityInput) error {
	if in.ReceiptHandle == s.handle {
		return s.faulty.ChangeVisibility(ctx, in)
	}
	return s.ok.ChangeVisibility(ctx, in)
}

func TestSQSChangeMessageVisibilityBatch_UncertainWriteIsRetryable(t *testing.T) {
	for name, landed := range map[string]bool{"write failed": false, "write applied but reply lost": true} {
		t.Run(name, func(t *testing.T) {
			a := newSQSRecvApp(t, allowAllECRDocs(), nil)
			u := a.create(t, "orders", nil)
			a.send(t, u, "first")
			a.send(t, u, "second")
			handles := a.receiveAll(t, u, 2, 300)
			a.owner.mu.Lock()
			a.owner.vis = splitVisibility{
				ok:     a.owner.vis,
				faulty: &message.VisibilityChanger{Store: recvFaultStore{Store: a.msgs, landed: landed}, Queues: a.store, Keys: gwReceiptKeys},
				handle: handles["second"],
			}
			a.owner.mu.Unlock()

			out, err := a.client().ChangeMessageVisibilityBatch(context.Background(), &sqs.ChangeMessageVisibilityBatchInput{
				QueueUrl: aws.String(u),
				Entries: []sqstypes.ChangeMessageVisibilityBatchRequestEntry{
					{Id: aws.String("a"), ReceiptHandle: aws.String(handles["first"])},
					{Id: aws.String("b"), ReceiptHandle: aws.String(handles["second"])},
				},
			})
			require.NoError(t, err)
			require.Len(t, out.Successful, 1)
			assert.Equal(t, "a", aws.ToString(out.Successful[0].Id))
			require.Len(t, out.Failed, 1)
			f := out.Failed[0]
			assert.Equal(t, "b", aws.ToString(f.Id))
			assert.False(t, f.SenderFault, "an unconfirmed change is retryable, not a client fault")
			assert.Equal(t, "ServiceUnavailable", aws.ToString(f.Code))
			want := []string{"first"}
			if landed {
				want = append(want, "second")
			}
			assert.ElementsMatch(t, want, a.visibleNow(t, u), "the reported failure says nothing about whether the write applied")
		})
	}
}
