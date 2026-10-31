// Drives ChangeMessageVisibility through the signed gateway against the real
// queue and message owners, reusing the receive harness.

package gateway

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const changeTarget = "AmazonSQS.ChangeMessageVisibility"

var wireNotInflight = wireError{400, "com.amazonaws.sqs#MessageNotInflight", "AWS.SimpleQueueService.MessageNotInflight;Sender", ""}

func changeBody(u, handle string, timeout int) []byte {
	return []byte(`{"QueueUrl":"` + u + `","ReceiptHandle":"` + handle + `","VisibilityTimeout":` + strconv.Itoa(timeout) + `}`)
}

func (a sqsRecvApp) change(u, handle string, timeout int32) error {
	_, err := a.client().ChangeMessageVisibility(context.Background(), &sqs.ChangeMessageVisibilityInput{
		QueueUrl: aws.String(u), ReceiptHandle: aws.String(handle), VisibilityTimeout: timeout,
	})
	return err
}

func (a sqsRecvApp) receiveOne(t *testing.T, u string, vis int32) sqstypes.Message {
	t.Helper()
	got := a.receive(t, &sqs.ReceiveMessageInput{
		QueueUrl: aws.String(u), VisibilityTimeout: vis,
		MessageSystemAttributeNames: []sqstypes.MessageSystemAttributeName{"ApproximateReceiveCount"},
	})
	require.Len(t, got, 1)
	return got[0]
}

func TestSQSChangeMessageVisibility_ReleaseAndExtend(t *testing.T) {
	a := newSQSRecvApp(t, allowAllECRDocs(), nil)
	u := a.create(t, "orders", nil)
	a.send(t, u, "hello")

	first := a.receiveOne(t, u, 300)
	assert.Empty(t, a.receive(t, &sqs.ReceiveMessageInput{QueueUrl: aws.String(u)}), "in flight for 300s")
	require.NoError(t, a.change(u, aws.ToString(first.ReceiptHandle), 0), "an explicit zero releases it")

	second := a.receiveOne(t, u, 1)
	assert.Equal(t, "hello", aws.ToString(second.Body))
	assert.Equal(t, "2", second.Attributes["ApproximateReceiveCount"])
	require.NoError(t, a.change(u, aws.ToString(second.ReceiptHandle), 60), "extended from 1s to 60s")
	require.Never(t, func() bool {
		return len(a.receive(t, &sqs.ReceiveMessageInput{QueueUrl: aws.String(u)})) > 0
	}, 2*time.Second, 200*time.Millisecond, "the extension holds past the original 1s deadline")

	var stale *sqstypes.MessageNotInflight
	assert.ErrorAs(t, a.change(u, aws.ToString(first.ReceiptHandle), 0), &stale, "the first receive's handle is stale")
}

func TestSQSChangeMessageVisibility_Handles(t *testing.T) {
	a := newSQSRecvApp(t, allowAllECRDocs(), nil)
	u := a.create(t, "orders", nil)
	other := a.create(t, "other", nil)
	a.send(t, u, "hello")
	h := aws.ToString(a.receiveOne(t, u, 300).ReceiptHandle)

	tampered := []byte(h)
	mid := len(tampered) / 2
	tampered[mid] = map[bool]byte{true: 'B', false: 'A'}[tampered[mid] == 'A']
	for name, tc := range map[string]struct {
		u, h string
		want wireError
	}{
		"tampered":    {u, string(tampered), wireInvalidHandle},
		"garbage":     {u, "not-a-handle", wireInvalidHandle},
		"other queue": {other, h, wireInvalidHandle},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, withoutMessage(readWireError(t, a.raw(t, changeTarget, changeBody(tc.u, tc.h, 10)))))
		})
	}
	var invalid *sqstypes.ReceiptHandleIsInvalid
	assert.ErrorAs(t, a.change(u, "not-a-handle", 10), &invalid)
	require.NoError(t, a.change(u, h, 300), "the genuine handle still works after the refusals")

	_, err := a.client().DeleteMessage(context.Background(), &sqs.DeleteMessageInput{QueueUrl: aws.String(u), ReceiptHandle: aws.String(h)})
	require.NoError(t, err)
	assert.Equal(t, wireNotInflight, withoutMessage(readWireError(t, a.raw(t, changeTarget, changeBody(u, h, 10)))), "a deleted message is not in flight")
}

// The 12-hour limit counts from the receive, not from the change; the wire
// code for exceeding it is provisional.
func TestSQSChangeMessageVisibility_TwelveHourLimit(t *testing.T) {
	a := newSQSRecvApp(t, allowAllECRDocs(), nil)
	u := a.create(t, "orders", nil)
	a.send(t, u, "hello")
	h := aws.ToString(a.receiveOne(t, u, 30).ReceiptHandle)
	require.NoError(t, a.change(u, h, 43000), "within 12 hours of the receive")
	assert.Equal(t, wireInvalidParam, withoutMessage(readWireError(t, a.raw(t, changeTarget, changeBody(u, h, 43200)))),
		"a full 12 hours from now ends after the receive's 12 hours")
	require.NoError(t, a.change(u, h, 0), "a shorter timeout within the limit still applies")
}

func TestSQSChangeMessageVisibility_Authorization(t *testing.T) {
	queueARN := "arn:aws:sqs:" + testRegion + ":" + ecrTestAccount + ":orders"
	setup := sqsPolicy([]string{"sqs:CreateQueue", "sqs:SendMessage", "sqs:ReceiveMessage"}, "*")
	for name, tc := range map[string]struct {
		docs []iamdomain.PolicyDocument
		ok   bool
	}{
		"other actions": {append(setup, sqsPolicy([]string{"sqs:DeleteMessage"}, "*")...), false},
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
			h := aws.ToString(a.receiveOne(t, u, 300).ReceiptHandle)
			resp := a.raw(t, changeTarget, changeBody(u, h, 0))
			if tc.ok {
				assert.Equal(t, http.StatusOK, resp.StatusCode)
				assert.Equal(t, 1, a.owner.changeCount())
				return
			}
			assert.Equal(t, wireDenied, withoutMessage(readWireError(t, resp)))
			absent := a.url + "/" + ecrTestAccount + "/orders-absent"
			assert.Equal(t, wireDenied, withoutMessage(readWireError(t, a.raw(t, changeTarget, changeBody(absent, "x", 0)))), "denial reveals neither the queue nor the handle")
			assert.Zero(t, a.owner.changeCount())
			assert.Empty(t, a.receive(t, &sqs.ReceiveMessageInput{QueueUrl: aws.String(u)}), "the denied release changed nothing")
		})
	}
}

// A failed or unacknowledged write is unavailable, never success, even when
// the write in fact landed.
func TestSQSChangeMessageVisibility_StoreFailureIsNeverSuccess(t *testing.T) {
	for name, landed := range map[string]bool{"write failed": false, "write reply lost after landing": true} {
		t.Run(name, func(t *testing.T) {
			a := newSQSRecvApp(t, allowAllECRDocs(), nil)
			u := a.create(t, "orders", nil)
			a.send(t, u, "hello")
			h := aws.ToString(a.receiveOne(t, u, 300).ReceiptHandle)
			a.owner.mu.Lock()
			a.owner.vis = &message.VisibilityChanger{Store: recvFaultStore{Store: a.msgs, landed: landed}, Queues: a.store, Keys: gwReceiptKeys}
			a.owner.mu.Unlock()
			assert.Equal(t, wireUnavailable, withoutMessage(readWireError(t, a.raw(t, changeTarget, changeBody(u, h, 0)))))
			released := len(a.receive(t, &sqs.ReceiveMessageInput{QueueUrl: aws.String(u)})) == 1
			assert.Equal(t, landed, released, "only a write that landed released the message")
		})
	}
}
