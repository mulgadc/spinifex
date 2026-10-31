package awsapi_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/aws/smithy-go"
	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/awsapi"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	sqsjs "github.com/mulgadc/spinifex/spinifex/domains/sqs/message/jetstream"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/queue"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/envelope"
)

const otherAccount = "210987654321"

// sdkApp serves the SQS registration to the AWS SDK over the real queue and
// message owners. The caller's account and the actions it may call are set
// per request; ingress authentication itself is not under test here.
type sdkApp struct {
	url string

	mu      sync.Mutex
	account string
	denied  map[string]bool
	asked   []string
}

func newSDKApp(t *testing.T) *sdkApp {
	t.Helper()
	ctx := context.Background()
	_, _, cpJS := testutil.StartTestJetStream(t)
	qs, err := queue.Open(ctx, cpJS, 3, 2)
	require.NoError(t, err)
	_, _, storeJS := testutil.StartTestJetStream(t)
	ms, err := sqsjs.New(ctx, storeJS, sqsjs.Config{MessageStreams: 3, StateBuckets: 2, DuplicateWindow: time.Minute})
	require.NoError(t, err)
	keys, err := message.NewReceiptKeys(1, map[uint32][]byte{1: bytes.Repeat([]byte{9}, message.MinReceiptKeyLen)})
	require.NoError(t, err)

	a := &sdkApp{account: testAccount, denied: map[string]bool{}}
	var reg dispatch.Registration
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		account := a.account
		a.mu.Unlock()
		err := reg.Dispatch(w, dispatch.Invocation{
			Request: r, AccountID: account, Region: testRegion, Service: awsapi.ServiceName,
			Authorize: a.authorize,
			Actor:     func() (dispatch.Actor, error) { return dispatch.Actor{UserID: "AIDAPOLLER", AccountID: account}, nil },
		})
		if err != nil {
			writeError(w, reg, err)
		}
	}))
	t.Cleanup(srv.Close)
	a.url = srv.URL
	reg = awsapi.NewRegistration(awsapi.Deps{
		Queues: qs, Messages: &message.Sender{Store: ms, Queues: qs},
		Receives:   &message.Receiver{Store: ms, Queues: qs, Keys: keys},
		Deletes:    &message.Deleter{Store: ms, Queues: qs, Keys: keys},
		Visibility: &message.VisibilityChanger{Store: ms, Queues: qs, Keys: keys},
		Endpoint:   srv.URL,
	})
	return a
}

func (a *sdkApp) authorize(_, action string, resources []string, _ iampolicy.ConditionKeys) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range resources {
		a.asked = append(a.asked, action+" "+r)
	}
	if a.denied[action] {
		return &dispatch.PolicyDenialError{Action: action}
	}
	return nil
}

func (a *sdkApp) as(account string, denied ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.account, a.denied, a.asked = account, map[string]bool{}, nil
	for _, d := range denied {
		a.denied[d] = true
	}
}

func (a *sdkApp) client() *sqs.Client {
	return sqs.New(sqs.Options{
		BaseEndpoint: aws.String(a.url), Region: testRegion, RetryMaxAttempts: 1,
		Credentials: aws.AnonymousCredentials{},
	})
}

// writeError renders a dispatch error as the JSON 1.0 query-compatible body
// the SDK decodes, using the registration's legacy query codes.
func writeError(w http.ResponseWriter, reg dispatch.Registration, err error) {
	code, msg, _ := awserrors.ResolveErrorDetail(err)
	if code == "" {
		code = err.Error()
	}
	meta := awserrors.LookupErrorMessage(awsapi.ServiceName, code)
	if msg == "" {
		msg = meta.Message
	}
	qc := code
	if q, ok := reg.QueryErrorCodes[code]; ok {
		qc = q
	}
	envelope.WriteJSON(w, envelope.ProtocolJSON10Query, envelope.JSONError{
		Code: code, Verbatim: true, Message: msg, Status: meta.HTTPCode, Namespace: reg.ErrorNamespace, QueryCode: qc,
	})
}

func apiCode(t *testing.T, err error) string {
	t.Helper()
	var ae smithy.APIError
	require.ErrorAs(t, err, &ae)
	return ae.ErrorCode()
}

// TestSDK_LambdaPollerPath drives the calls an event source mapping makes, in
// order, through the registration and the real queue and message owners.
func TestSDK_LambdaPollerPath(t *testing.T) {
	a := newSDKApp(t)
	c := a.client()
	ctx := context.Background()

	created, err := c.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String("lambda-src"),
		Attributes: map[string]string{"VisibilityTimeout": "45"}})
	require.NoError(t, err)
	got, err := c.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String("lambda-src")})
	require.NoError(t, err)
	u := aws.ToString(got.QueueUrl)
	require.Equal(t, aws.ToString(created.QueueUrl), u)

	attrs, err := c.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: aws.String(u),
		AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameQueueArn, sqstypes.QueueAttributeNameVisibilityTimeout}})
	require.NoError(t, err)
	assert.Equal(t, "arn:aws:sqs:"+testRegion+":"+testAccount+":lambda-src", attrs.Attributes["QueueArn"])
	assert.Equal(t, "45", attrs.Attributes["VisibilityTimeout"])

	entries := make([]sqstypes.SendMessageBatchRequestEntry, 3)
	for i := range entries {
		entries[i] = sqstypes.SendMessageBatchRequestEntry{Id: aws.String("e" + strconv.Itoa(i)), MessageBody: aws.String("job-" + strconv.Itoa(i)),
			MessageAttributes: map[string]sqstypes.MessageAttributeValue{"kind": {DataType: aws.String("String"), StringValue: aws.String("job")}}}
	}
	sent, err := c.SendMessageBatch(ctx, &sqs.SendMessageBatchInput{QueueUrl: aws.String(u), Entries: entries})
	require.NoError(t, err)
	require.Len(t, sent.Successful, 3)
	require.Empty(t, sent.Failed)

	recv, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(u), MaxNumberOfMessages: 10,
		WaitTimeSeconds: 20, MessageAttributeNames: []string{"All"},
		MessageSystemAttributeNames: []sqstypes.MessageSystemAttributeName{sqstypes.MessageSystemAttributeNameAll}})
	require.NoError(t, err)
	require.Len(t, recv.Messages, 3)
	bodies := map[string]bool{}
	for _, m := range recv.Messages {
		bodies[aws.ToString(m.Body)] = true
		assert.Equal(t, "job", aws.ToString(m.MessageAttributes["kind"].StringValue))
		assert.NotEmpty(t, m.Attributes["SenderId"])
		assert.Equal(t, "1", m.Attributes["ApproximateReceiveCount"])
		assert.NotEmpty(t, m.Attributes["ApproximateFirstReceiveTimestamp"])
	}
	assert.Equal(t, map[string]bool{"job-0": true, "job-1": true, "job-2": true}, bodies)

	// A failed record goes back to the queue; the rest are deleted in a batch.
	failed := recv.Messages[0]
	vis, err := c.ChangeMessageVisibilityBatch(ctx, &sqs.ChangeMessageVisibilityBatchInput{QueueUrl: aws.String(u),
		Entries: []sqstypes.ChangeMessageVisibilityBatchRequestEntry{{Id: aws.String("f"), ReceiptHandle: failed.ReceiptHandle, VisibilityTimeout: 0}}})
	require.NoError(t, err)
	require.Len(t, vis.Successful, 1)
	del := make([]sqstypes.DeleteMessageBatchRequestEntry, 0, 2)
	for i, m := range recv.Messages[1:] {
		del = append(del, sqstypes.DeleteMessageBatchRequestEntry{Id: aws.String("d" + strconv.Itoa(i)), ReceiptHandle: m.ReceiptHandle})
	}
	dr, err := c.DeleteMessageBatch(ctx, &sqs.DeleteMessageBatchInput{QueueUrl: aws.String(u), Entries: del})
	require.NoError(t, err)
	require.Len(t, dr.Successful, 2)
	require.Empty(t, dr.Failed)

	again, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(u), MaxNumberOfMessages: 10, WaitTimeSeconds: 1})
	require.NoError(t, err)
	require.Len(t, again.Messages, 1, "only the released record is redelivered")
	assert.Equal(t, aws.ToString(failed.Body), aws.ToString(again.Messages[0].Body))
	_, err = c.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(u), ReceiptHandle: again.Messages[0].ReceiptHandle})
	require.NoError(t, err)

	start := time.Now()
	empty, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(u), WaitTimeSeconds: 2})
	require.NoError(t, err)
	assert.Empty(t, empty.Messages)
	assert.GreaterOrEqual(t, time.Since(start), 2*time.Second, "an empty long poll waits its full time")
}

// TestSDK_AuthorizationAndIsolation checks that each call is authorized on the
// queue ARN, that a denial changes nothing, and that one account cannot reach
// another account's queue by URL.
func TestSDK_AuthorizationAndIsolation(t *testing.T) {
	a := newSDKApp(t)
	c := a.client()
	ctx := context.Background()
	created, err := c.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String("jobs")})
	require.NoError(t, err)
	u := aws.ToString(created.QueueUrl)
	arn := "arn:aws:sqs:" + testRegion + ":" + testAccount + ":jobs"

	a.as(testAccount, "SendMessage")
	_, err = c.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(u), MessageBody: aws.String("x")})
	assert.Equal(t, awserrors.ErrorAccessDenied, apiCode(t, err))
	assert.Equal(t, []string{"SendMessage " + arn}, a.asked)

	a.as(otherAccount)
	_, err = c.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(u), MessageBody: aws.String("x")})
	assert.Equal(t, awserrors.ErrorSQSInvalidAddress, apiCode(t, err))
	assert.Empty(t, a.asked, "a foreign queue URL is refused before authorization")
	_, err = c.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String("jobs")})
	var missing *sqstypes.QueueDoesNotExist
	assert.ErrorAs(t, err, &missing, "another account sees no queue named jobs")

	a.as(testAccount)
	out, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(u), WaitTimeSeconds: 0})
	require.NoError(t, err)
	assert.Empty(t, out.Messages, "neither refused send reached the queue")
	assert.Equal(t, []string{"ReceiveMessage " + arn}, a.asked)
}

// TestSDK_DeleteBatchReportsEachEntry checks that a batch with a valid and a
// tampered handle deletes the valid one and reports the other as failed.
func TestSDK_DeleteBatchReportsEachEntry(t *testing.T) {
	a := newSDKApp(t)
	c := a.client()
	ctx := context.Background()
	created, err := c.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String("jobs")})
	require.NoError(t, err)
	u := aws.ToString(created.QueueUrl)
	for _, b := range []string{"a", "b"} {
		_, err = c.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(u), MessageBody: aws.String(b)})
		require.NoError(t, err)
	}
	recv, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(u), MaxNumberOfMessages: 10, VisibilityTimeout: 1})
	require.NoError(t, err)
	require.Len(t, recv.Messages, 2)
	kept := recv.Messages[1]
	// Flip a middle character, not the last, whose low bits base64 may drop.
	h := []byte(aws.ToString(kept.ReceiptHandle))
	i := len(h) / 2
	if h[i] == 'A' {
		h[i] = 'B'
	} else {
		h[i] = 'A'
	}
	tampered := string(h)
	require.NotEqual(t, aws.ToString(kept.ReceiptHandle), tampered)

	dr, err := c.DeleteMessageBatch(ctx, &sqs.DeleteMessageBatchInput{QueueUrl: aws.String(u), Entries: []sqstypes.DeleteMessageBatchRequestEntry{
		{Id: aws.String("ok"), ReceiptHandle: recv.Messages[0].ReceiptHandle},
		{Id: aws.String("bad"), ReceiptHandle: aws.String(tampered)},
	}})
	require.NoError(t, err)
	require.Len(t, dr.Successful, 1)
	assert.Equal(t, "ok", aws.ToString(dr.Successful[0].Id))
	require.Len(t, dr.Failed, 1)
	assert.Equal(t, "bad", aws.ToString(dr.Failed[0].Id))
	assert.True(t, dr.Failed[0].SenderFault)
	assert.Contains(t, aws.ToString(dr.Failed[0].Code), "ReceiptHandleIsInvalid")
	assert.NotContains(t, aws.ToString(dr.Failed[0].Message), tampered, "a handle is never echoed")

	again, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(u), MaxNumberOfMessages: 10, WaitTimeSeconds: 3})
	require.NoError(t, err)
	require.Len(t, again.Messages, 1, "only the entry that failed to delete returns")
	assert.Equal(t, aws.ToString(kept.Body), aws.ToString(again.Messages[0].Body))
}
