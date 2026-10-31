package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/mulgadc/spinifex/internal/testkit"
	sqsawsapi "github.com/mulgadc/spinifex/spinifex/domains/sqs/awsapi"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/queue"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const setTarget = "AmazonSQS.SetQueueAttributes"

func (a sqsApp) record(t *testing.T, name string) queue.Record {
	t.Helper()
	r, err := a.store.Resolve(context.Background(), ecrTestAccount, testRegion, name)
	require.NoError(t, err)
	return r
}

func setBody(u, attrs string) []byte {
	return []byte(`{"QueueUrl":"` + u + `","Attributes":` + attrs + `}`)
}

func TestSQSSetQueueAttributes_SDK(t *testing.T) {
	a := newSQSApp(t, allowAllECRDocs(), true)
	ctx := context.Background()
	u := a.create(t, "orders", nil)
	before := a.record(t, "orders")

	_, err := a.client().SetQueueAttributes(ctx, &sqs.SetQueueAttributesInput{
		QueueUrl:   aws.String(u),
		Attributes: map[string]string{"VisibilityTimeout": "45", "DelaySeconds": "5"},
	})
	require.NoError(t, err)

	out, err := a.client().GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(u),
		AttributeNames: []sqstypes.QueueAttributeName{"VisibilityTimeout", "DelaySeconds", "MessageRetentionPeriod"},
	})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"VisibilityTimeout": "45", "DelaySeconds": "5", "MessageRetentionPeriod": "345600"}, out.Attributes)
	after := a.record(t, "orders")
	assert.Equal(t, before.Generation+1, after.Generation)
	assert.Equal(t, before.QueueID, after.QueueID)

	// A same-value call is still an accepted mutation.
	_, err = a.client().SetQueueAttributes(ctx, &sqs.SetQueueAttributesInput{
		QueueUrl: aws.String(u), Attributes: map[string]string{"VisibilityTimeout": "45"},
	})
	require.NoError(t, err)
	assert.Equal(t, after.Generation+1, a.record(t, "orders").Generation)
}

func TestSQSSetQueueAttributes_AllOrNothing(t *testing.T) {
	a := newSQSApp(t, allowAllECRDocs(), true)
	u := a.create(t, "orders", nil)
	before := a.record(t, "orders")
	for name, tc := range map[string]struct {
		attrs string
		want  wireError
	}{
		"one value out of range": {`{"VisibilityTimeout":"45","DelaySeconds":"901"}`,
			wireError{400, "com.amazonaws.sqs#InvalidAttributeValue", "InvalidAttributeValue;Sender", ""}},
		"unknown name": {`{"VisibilityTimeout":"45","Bogus":"1"}`,
			wireError{400, "com.amazonaws.sqs#InvalidAttributeName", "InvalidAttributeName;Sender", ""}},
		"read-only name": {`{"VisibilityTimeout":"45","QueueArn":"x"}`,
			wireError{400, "com.amazonaws.sqs#InvalidAttributeName", "InvalidAttributeName;Sender", ""}},
		"unsupported policy": {`{"VisibilityTimeout":"45","Policy":"{}"}`,
			wireError{400, "com.amazonaws.sqs#UnsupportedOperation", "AWS.SimpleQueueService.UnsupportedOperation;Sender", ""}},
		"unsupported redrive": {`{"RedrivePolicy":"{}"}`,
			wireError{400, "com.amazonaws.sqs#UnsupportedOperation", "AWS.SimpleQueueService.UnsupportedOperation;Sender", ""}},
		"fifo": {`{"FifoQueue":"true"}`,
			wireError{400, "com.amazonaws.sqs#InvalidAttributeName", "InvalidAttributeName;Sender", ""}},
	} {
		t.Run(name, func(t *testing.T) {
			got := readWireError(t, a.raw(t, setTarget, setBody(u, tc.attrs)))
			want := tc.want
			want.message = got.message
			assert.Equal(t, want, got)
			assert.Equal(t, before, a.record(t, "orders"), "a rejected request writes nothing")
		})
	}
}

func TestSQSSetQueueAttributes_RequestShape(t *testing.T) {
	a := newSQSApp(t, allowAllECRDocs(), true)
	u := a.create(t, "orders", nil)
	before := a.record(t, "orders")
	missingAttrs := wireError{400, "com.amazon.coral.service#MissingRequiredParameterException", "MissingParameter;Sender",
		"The request must contain the parameter Attributes."}
	for name, tc := range map[string]struct {
		body string
		want wireError
	}{
		"no URL": {`{"Attributes":{"DelaySeconds":"1"}}`, wireError{400, "com.amazon.coral.service#MissingRequiredParameterException",
			"MissingParameter;Sender", "The request must contain the parameter QueueUrl."}},
		"no attributes":    {`{"QueueUrl":"` + u + `"}`, missingAttrs},
		"empty attributes": {string(setBody(u, `{}`)), missingAttrs},
		"wrong host": {string(setBody("http://localhost:1/"+ecrTestAccount+"/orders", `{"DelaySeconds":"1"}`)),
			wireError{404, "com.amazonaws.sqs#InvalidAddress", "InvalidAddress;Sender", ""}},
		"another account": {string(setBody(a.url+"/"+otherOwner+"/orders", `{"DelaySeconds":"1"}`)),
			wireError{404, "com.amazonaws.sqs#InvalidAddress", "InvalidAddress;Sender", ""}},
		"trailing input": {string(setBody(u, `{"DelaySeconds":"1"}`)) + "}",
			wireError{400, "com.amazon.coral.service#SerializationException", "MalformedInput;Sender", ""}},
	} {
		t.Run(name, func(t *testing.T) {
			got := readWireError(t, a.raw(t, setTarget, []byte(tc.body)))
			want := tc.want
			if want.message == "" {
				want.message = got.message
			}
			assert.Equal(t, want, got)
			assert.Equal(t, before, a.record(t, "orders"))
		})
	}
}

// Another account's same-named queue is never reached through the caller's URL.
func TestSQSSetQueueAttributes_AccountBoundary(t *testing.T) {
	a := newSQSApp(t, allowAllECRDocs(), true)
	ctx := context.Background()
	a.seedElsewhere(t, "orders")
	foreign, err := a.store.Resolve(ctx, otherOwner, testRegion, "orders")
	require.NoError(t, err)

	got := readWireError(t, a.raw(t, setTarget, setBody(a.urlOf("orders"), `{"DelaySeconds":"1"}`)))
	assert.Equal(t, notExist(got), got)
	after, err := a.store.Resolve(ctx, otherOwner, testRegion, "orders")
	require.NoError(t, err)
	assert.Equal(t, foreign, after)
}

func TestSQSSetQueueAttributes_Authorization(t *testing.T) {
	t.Run("denied without the action, nothing written", func(t *testing.T) {
		a := newSQSApp(t, sqsPolicy([]string{"sqs:CreateQueue", "sqs:GetQueueAttributes"}, "*"), true)
		u := a.create(t, "orders", nil)
		before := a.record(t, "orders")
		got := readWireError(t, a.raw(t, setTarget, setBody(u, `{"DelaySeconds":"1"}`)))
		assert.Equal(t, wireError{403, "com.amazon.coral.service#AccessDeniedException", "AccessDenied;Sender", got.message}, got)
		assert.Equal(t, before, a.record(t, "orders"))
	})

	t.Run("a grant on another queue does not apply", func(t *testing.T) {
		a := newSQSApp(t, sqsPolicy([]string{"sqs:SetQueueAttributes"}, "arn:aws:sqs:"+testRegion+":"+ecrTestAccount+":other"), true)
		got := readWireError(t, a.raw(t, setTarget, setBody(a.urlOf("orders"), `{"DelaySeconds":"1"}`)))
		assert.Equal(t, 403, got.status)
	})

	t.Run("denial does not reveal whether the queue exists", func(t *testing.T) {
		a := newSQSApp(t, sqsPolicy([]string{"sqs:CreateQueue"}, "*"), true)
		a.create(t, "orders", nil)
		present := readWireError(t, a.raw(t, setTarget, setBody(a.urlOf("orders"), `{"DelaySeconds":"1"}`)))
		absent := readWireError(t, a.raw(t, setTarget, setBody(a.urlOf("nope"), `{"DelaySeconds":"1"}`)))
		assert.Equal(t, 403, present.status)
		assert.Equal(t, present.typ, absent.typ)
	})

	t.Run("invalid attributes are rejected before authorizing", func(t *testing.T) {
		a := newSQSApp(t, sqsPolicy([]string{"sqs:CreateQueue"}, "*"), true)
		got := readWireError(t, a.raw(t, setTarget, setBody(a.urlOf("orders"), `{"DelaySeconds":"901"}`)))
		assert.Equal(t, "com.amazonaws.sqs#InvalidAttributeValue", got.typ)
	})

	t.Run("queue-scoped grant allows", func(t *testing.T) {
		a := newSQSApp(t, sqsPolicy([]string{"sqs:SetQueueAttributes", "sqs:CreateQueue"}, "arn:aws:sqs:"+testRegion+":"+ecrTestAccount+":orders"), true)
		u := a.create(t, "orders", nil)
		resp := a.raw(t, setTarget, setBody(u, `{"DelaySeconds":"1"}`))
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "1", a.record(t, "orders").Attributes[queue.AttrDelaySeconds])
	})
}

// lostReplyQueues applies the update and then reports it as unavailable, as
// when a write's reply is lost after it landed.
type lostReplyQueues struct{ *queue.Store }

func (q lostReplyQueues) SetAttributes(ctx context.Context, account, region, name string, attrs map[string]string) (queue.Record, error) {
	if _, err := q.Store.SetAttributes(ctx, account, region, name, attrs); err != nil {
		return queue.Record{}, err
	}
	return queue.Record{}, errors.Join(queue.ErrUnavailable, errors.New("reply lost"))
}

// An unprovable outcome is 503, never success; a client retry is a separate
// accepted request and advances the generation again.
func TestSQSSetQueueAttributes_LostReply(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	store, err := queue.Open(context.Background(), js, 16, 8)
	require.NoError(t, err)
	var app http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { app.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	_, app = dispatchSeamApp(t, allowAllECRDocs(), sqsawsapi.NewRegistration(sqsawsapi.Deps{Queues: lostReplyQueues{store}, Endpoint: srv.URL}))
	a := sqsApp{url: srv.URL, store: store}

	rec, err := store.Create(context.Background(), queue.CreateRequest{Account: ecrTestAccount, Region: testRegion, Name: "orders"})
	require.NoError(t, err)
	for i := uint64(1); i <= 2; i++ {
		got := readWireError(t, a.raw(t, setTarget, setBody(a.urlOf("orders"), `{"DelaySeconds":"7"}`)))
		assert.Equal(t, wireError{503, "com.amazon.coral.service#ServiceUnavailableException", "ServiceUnavailable;Receiver", got.message}, got)
		after := a.record(t, "orders")
		assert.Equal(t, rec.Generation+i, after.Generation)
		assert.Equal(t, "7", after.Attributes[queue.AttrDelaySeconds])
	}
}
