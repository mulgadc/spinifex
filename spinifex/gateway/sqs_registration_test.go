// Drives the SQS JSON 1.0 registration through the signed gateway pipeline
// against the real queue owner on an embedded JetStream, with both raw
// requests and the AWS SDK v2 SQS client.

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	"github.com/mulgadc/spinifex/internal/testkit"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	sqsawsapi "github.com/mulgadc/spinifex/spinifex/domains/sqs/awsapi"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/queue"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type sqsApp struct {
	url   string
	store *queue.Store
	cfg   aws.Config
}

// newSQSApp serves the SQS registration on a test server whose own URL is
// the queue-URL endpoint. A nil store composes the adapter without one.
func newSQSApp(t *testing.T, docs []iamdomain.PolicyDocument, withStore bool) sqsApp {
	t.Helper()
	var store *queue.Store
	if withStore {
		_, _, js := testutil.StartTestJetStream(t)
		var err error
		store, err = queue.Open(context.Background(), js, 16, 8)
		require.NoError(t, err)
	}
	var app http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { app.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	deps := sqsawsapi.Deps{Endpoint: srv.URL}
	if store != nil {
		deps.Queues = store
	}
	_, app = dispatchSeamApp(t, docs, sqsawsapi.NewRegistration(deps))
	return sqsApp{url: srv.URL, store: store, cfg: sdkConfig(srv.URL)}
}

func (a sqsApp) client() *sqs.Client { return sqs.NewFromConfig(a.cfg) }

// raw sends a signed JSON 1.0 request with the given target and body.
func (a sqsApp) raw(t *testing.T, target string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, a.url+"/", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	if target != "" {
		req.Header.Set("X-Amz-Target", target)
	}
	signTestRequestAs(t, req, body, testAccessKey, testSecretKey, "sqs", time.Now().UTC())
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func (a sqsApp) exists(t *testing.T, name string) bool {
	t.Helper()
	_, err := a.store.Resolve(context.Background(), ecrTestAccount, testRegion, name)
	if errors.Is(err, queue.ErrQueueDoesNotExist) {
		return false
	}
	require.NoError(t, err)
	return true
}

type wireError struct {
	status     int
	typ        string
	queryError string
	message    string
}

func readWireError(t *testing.T, resp *http.Response) wireError {
	t.Helper()
	assert.Equal(t, "application/x-amz-json-1.0", resp.Header.Get("Content-Type"))
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var body map[string]string
	require.NoError(t, json.Unmarshal(b, &body), string(b))
	return wireError{status: resp.StatusCode, typ: body["__type"], queryError: resp.Header.Get("X-Amzn-Query-Error"), message: body["message"]}
}

func sqsPolicy(actions []string, resources ...string) []iamdomain.PolicyDocument {
	return []iamdomain.PolicyDocument{{
		Version: "2012-10-17",
		Statement: []iamdomain.Statement{
			{Effect: "Allow", Action: iamdomain.StringOrArr(actions), Resource: iamdomain.StringOrArr(resources)},
		},
	}}
}

const createTarget = "AmazonSQS.CreateQueue"

func TestSQSCreateQueue_SDKCreatesAndRepeats(t *testing.T) {
	a := newSQSApp(t, allowAllECRDocs(), true)
	ctx := context.Background()

	out, err := a.client().CreateQueue(ctx, &sqs.CreateQueueInput{
		QueueName:  aws.String("orders"),
		Attributes: map[string]string{"VisibilityTimeout": "45"},
	})
	require.NoError(t, err)
	assert.Equal(t, a.url+"/"+ecrTestAccount+"/orders", aws.ToString(out.QueueUrl))

	rec, err := a.store.Resolve(ctx, ecrTestAccount, testRegion, "orders")
	require.NoError(t, err)
	assert.Equal(t, "45", rec.Attributes["VisibilityTimeout"])
	assert.Equal(t, arn.FormatSQSQueue(testRegion, ecrTestAccount, "orders"), rec.ARN)

	again, err := a.client().CreateQueue(ctx, &sqs.CreateQueueInput{
		QueueName:  aws.String("orders"),
		Attributes: map[string]string{"VisibilityTimeout": "45"},
	})
	require.NoError(t, err)
	assert.Equal(t, aws.ToString(out.QueueUrl), aws.ToString(again.QueueUrl))

	_, err = a.client().CreateQueue(ctx, &sqs.CreateQueueInput{
		QueueName:  aws.String("orders"),
		Attributes: map[string]string{"VisibilityTimeout": "46"},
	})
	var exists *sqstypes.QueueNameExists
	require.ErrorAs(t, err, &exists)

	resp := a.raw(t, createTarget, []byte(`{"QueueName":"orders","Attributes":{"VisibilityTimeout":"46"}}`))
	got := readWireError(t, resp)
	assert.Equal(t, wireError{status: 400, typ: "com.amazonaws.sqs#QueueNameExists", queryError: "QueueAlreadyExists;Sender", message: got.message}, got)
}

func TestSQSCreateQueue_SuccessIsJSON10(t *testing.T) {
	a := newSQSApp(t, allowAllECRDocs(), true)
	resp := a.raw(t, createTarget, []byte(`{"QueueName":"plain"}`))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/x-amz-json-1.0", resp.Header.Get("Content-Type"))
	var out map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	assert.Equal(t, map[string]string{"QueueUrl": a.url + "/" + ecrTestAccount + "/plain"}, out)
}

// The captured AWS responses for these cases are in the SQS error wire
// report; the provisional rows are marked.
func TestSQSCreateQueue_ValidationEnvelopes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want wireError
	}{
		{"VisibilityTimeout out of range", `{"QueueName":"q","Attributes":{"VisibilityTimeout":"99999"}}`,
			wireError{400, "com.amazonaws.sqs#InvalidAttributeValue", "InvalidAttributeValue;Sender", ""}},
		{"unknown attribute name", `{"QueueName":"q","Attributes":{"Bogus":"1"}}`,
			wireError{400, "com.amazonaws.sqs#InvalidAttributeName", "InvalidAttributeName;Sender", ""}},
		{"name with spaces and !", `{"QueueName":"bad name!"}`,
			wireError{400, "com.amazon.coral.service#InvalidParameterValueException", "InvalidParameterValue;Sender", ""}},
		{"missing QueueName", `{}`,
			wireError{400, "com.amazon.coral.service#MissingRequiredParameterException", "MissingParameter;Sender", "The request must contain the parameter QueueName."}},
		{"empty QueueName", `{"QueueName":""}`,
			wireError{400, "com.amazon.coral.service#MissingRequiredParameterException", "MissingParameter;Sender", "The request must contain the parameter QueueName."}},
		{"trailing data after the object", `{"QueueName":"q"}{}`,
			wireError{400, "com.amazon.coral.service#SerializationException", "MalformedInput;Sender", ""}},
		{"trailing close brace", `{"QueueName":"q"}}`,
			wireError{400, "com.amazon.coral.service#SerializationException", "MalformedInput;Sender", ""}},
		{"trailing close bracket", `{"QueueName":"q"}]`,
			wireError{400, "com.amazon.coral.service#SerializationException", "MalformedInput;Sender", ""}},
		{"trailing scalar", `{"QueueName":"q"} 1`,
			wireError{400, "com.amazon.coral.service#SerializationException", "MalformedInput;Sender", ""}},
		{"null body", `null`,
			wireError{400, "com.amazon.coral.service#SerializationException", "MalformedInput;Sender", ""}},
		{"array body", `[{"QueueName":"q"}]`,
			wireError{400, "com.amazon.coral.service#SerializationException", "MalformedInput;Sender", ""}},
		{"string body", `"q"`,
			wireError{400, "com.amazon.coral.service#SerializationException", "MalformedInput;Sender", ""}},
		{"not JSON", `{not json`,
			wireError{400, "com.amazon.coral.service#SerializationException", "MalformedInput;Sender", ""}},
		{"QueueName of the wrong type", `{"QueueName":7}`,
			wireError{400, "com.amazon.coral.service#SerializationException", "MalformedInput;Sender", ""}},
		// Provisional: the error code for rejected attributes is not captured.
		{"FIFO rejected", `{"QueueName":"q","Attributes":{"FifoQueue":"true"}}`,
			wireError{400, "com.amazonaws.sqs#UnsupportedOperation", "AWS.SimpleQueueService.UnsupportedOperation;Sender", ""}},
		// Provisional: the tag-limit error code is not captured.
		{"aws: tag prefix", `{"QueueName":"q","tags":{"aws:owner":"x"}}`,
			wireError{400, "com.amazon.coral.service#InvalidParameterValueException", "InvalidParameterValue;Sender", ""}},
	}
	a := newSQSApp(t, allowAllECRDocs(), true)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := readWireError(t, a.raw(t, createTarget, []byte(tc.body)))
			if tc.want.message == "" {
				tc.want.message = got.message
			}
			assert.Equal(t, tc.want, got)
			assert.False(t, a.exists(t, "q"))
		})
	}

	t.Run("SDK decodes the modelled errors", func(t *testing.T) {
		_, err := a.client().CreateQueue(context.Background(), &sqs.CreateQueueInput{
			QueueName: aws.String("q"), Attributes: map[string]string{"VisibilityTimeout": "99999"},
		})
		var bad *sqstypes.InvalidAttributeValue
		require.ErrorAs(t, err, &bad)
		_, err = a.client().CreateQueue(context.Background(), &sqs.CreateQueueInput{
			QueueName: aws.String("q"), Attributes: map[string]string{"FifoQueue": "true"},
		})
		var unsupported *sqstypes.UnsupportedOperation
		require.ErrorAs(t, err, &unsupported)
	})
}

func TestSQSCreateQueue_AuthorizesOnTheQueueARN(t *testing.T) {
	ordersARN := arn.FormatSQSQueue(testRegion, ecrTestAccount, "orders")

	t.Run("grant on another queue is denied and creates nothing", func(t *testing.T) {
		a := newSQSApp(t, sqsPolicy([]string{"sqs:CreateQueue"}, arn.FormatSQSQueue(testRegion, ecrTestAccount, "other")), true)
		got := readWireError(t, a.raw(t, createTarget, []byte(`{"QueueName":"orders"}`)))
		assert.Equal(t, wireError{403, "com.amazon.coral.service#AccessDeniedException", "AccessDenied;Sender", got.message}, got)
		assert.False(t, a.exists(t, "orders"))
	})

	t.Run("grant on the queue ARN allows", func(t *testing.T) {
		a := newSQSApp(t, sqsPolicy([]string{"sqs:CreateQueue"}, ordersARN), true)
		_, err := a.client().CreateQueue(context.Background(), &sqs.CreateQueueInput{QueueName: aws.String("orders")})
		require.NoError(t, err)
		assert.True(t, a.exists(t, "orders"))
	})

	t.Run("tags on create also need sqs:TagQueue", func(t *testing.T) {
		a := newSQSApp(t, sqsPolicy([]string{"sqs:CreateQueue"}, ordersARN), true)
		got := readWireError(t, a.raw(t, createTarget, []byte(`{"QueueName":"orders","tags":{"team":"a"}}`)))
		assert.Equal(t, 403, got.status)
		assert.False(t, a.exists(t, "orders"))
	})

	t.Run("tags on create with both grants are stored", func(t *testing.T) {
		a := newSQSApp(t, sqsPolicy([]string{"sqs:CreateQueue", "sqs:TagQueue"}, ordersARN), true)
		_, err := a.client().CreateQueue(context.Background(), &sqs.CreateQueueInput{
			QueueName: aws.String("orders"), Tags: map[string]string{"team": "a"},
		})
		require.NoError(t, err)
		tags, err := a.store.Tags(context.Background(), ecrTestAccount, testRegion, "orders")
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"team": "a"}, tags)
	})
}

func TestSQSCreateQueue_DeletedRecently(t *testing.T) {
	a := newSQSApp(t, allowAllECRDocs(), true)
	ctx := context.Background()
	_, err := a.client().CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String("gone")})
	require.NoError(t, err)
	rec, err := a.store.MarkDeleting(ctx, ecrTestAccount, testRegion, "gone")
	require.NoError(t, err)
	require.NoError(t, a.store.Reserve(ctx, ecrTestAccount, rec.QueueID))

	_, err = a.client().CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String("gone")})
	var recent *sqstypes.QueueDeletedRecently
	require.ErrorAs(t, err, &recent)
	got := readWireError(t, a.raw(t, createTarget, []byte(`{"QueueName":"gone"}`)))
	assert.Equal(t, wireError{400, "com.amazonaws.sqs#QueueDeletedRecently", "AWS.SimpleQueueService.QueueDeletedRecently;Sender", got.message}, got)
}

func TestSQSUnknownOperation(t *testing.T) {
	a := newSQSApp(t, allowAllECRDocs(), true)
	for name, target := range map[string]string{
		"unregistered operation": "AmazonSQS.PurgeQueue",
		"wrong prefix":           "AmazonSQSv2.CreateQueue",
		"bare operation":         "CreateQueue",
		"query protocol":         "",
	} {
		t.Run(name, func(t *testing.T) {
			got := readWireError(t, a.raw(t, target, []byte(`{"QueueName":"q"}`)))
			assert.Equal(t, wireError{400, "com.amazon.coral.service#UnknownOperationException", "InvalidAction;Sender", got.message}, got)
			assert.False(t, a.exists(t, "q"))
		})
	}
}

// unavailableQueues is a queue owner whose store cannot decide.
type unavailableQueues struct{}

func (unavailableQueues) Create(context.Context, queue.CreateRequest) (queue.Record, error) {
	return queue.Record{}, queue.ErrUnavailable
}

func (unavailableQueues) Resolve(context.Context, string, string, string) (queue.Record, error) {
	return queue.Record{}, queue.ErrUnavailable
}

func (unavailableQueues) List(context.Context, queue.ListRequest) (queue.ListResult, error) {
	return queue.ListResult{}, queue.ErrUnavailable
}

func (unavailableQueues) SetAttributes(context.Context, string, string, string, map[string]string) (queue.Record, error) {
	return queue.Record{}, queue.ErrUnavailable
}

func (unavailableQueues) Tag(context.Context, string, string, string, map[string]string) (queue.Record, error) {
	return queue.Record{}, queue.ErrUnavailable
}

func (unavailableQueues) Untag(context.Context, string, string, string, []string) (queue.Record, error) {
	return queue.Record{}, queue.ErrUnavailable
}

func (unavailableQueues) Tags(context.Context, string, string, string) (map[string]string, error) {
	return nil, queue.ErrUnavailable
}

func TestSQSCreateQueue_OwnerUnavailable(t *testing.T) {
	var app http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { app.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	_, app = dispatchSeamApp(t, allowAllECRDocs(), sqsawsapi.NewRegistration(sqsawsapi.Deps{Queues: unavailableQueues{}, Endpoint: srv.URL}))
	a := sqsApp{url: srv.URL}
	got := readWireError(t, a.raw(t, createTarget, []byte(`{"QueueName":"q"}`)))
	assert.Equal(t, wireError{503, "com.amazon.coral.service#ServiceUnavailableException", "ServiceUnavailable;Receiver", got.message}, got)
}

func conditioned(effect, op, key, value string, actions ...string) iamdomain.Statement {
	return iamdomain.Statement{
		Effect:    effect,
		Action:    iamdomain.StringOrArr(actions),
		Resource:  iamdomain.StringOrArr{"*"},
		Condition: map[string]map[string]iampolicy.ConditionValue{op: {key: {value}}},
	}
}

// The tag condition keys AWS lists for CreateQueue and TagQueue are not
// enforced by the IAM evaluator, which fails closed on them: a Deny carrying
// one applies whatever the request's tags, and an Allow carrying one grants
// nothing. Omitting the context therefore cannot bypass a tag-scoped Deny.
func TestSQSCreateQueue_TagConditionsFailClosed(t *testing.T) {
	allowAll := iamdomain.Statement{Effect: "Allow", Action: iamdomain.StringOrArr{"*"}, Resource: iamdomain.StringOrArr{"*"}}
	cases := []struct {
		name string
		stmt iamdomain.Statement
		body string
	}{
		{"Deny on aws:RequestTag applies to the matching tag", conditioned("Deny", "StringEquals", "aws:RequestTag/team", "blocked", "sqs:CreateQueue"),
			`{"QueueName":"q","tags":{"team":"blocked"}}`},
		{"Deny on aws:RequestTag also applies without tags", conditioned("Deny", "StringEquals", "aws:RequestTag/team", "blocked", "sqs:CreateQueue"),
			`{"QueueName":"q"}`},
		{"Deny on aws:TagKeys applies to TagQueue on create", conditioned("Deny", "ForAnyValue:StringEquals", "aws:TagKeys", "team", "sqs:TagQueue"),
			`{"QueueName":"q","tags":{"team":"a"}}`},
		{"Deny on aws:ResourceTag applies", conditioned("Deny", "StringEquals", "aws:ResourceTag/env", "prod", "sqs:CreateQueue"),
			`{"QueueName":"q"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			docs := []iamdomain.PolicyDocument{{Version: "2012-10-17", Statement: []iamdomain.Statement{allowAll, tc.stmt}}}
			a := newSQSApp(t, docs, true)
			got := readWireError(t, a.raw(t, createTarget, []byte(tc.body)))
			assert.Equal(t, 403, got.status)
			assert.False(t, a.exists(t, "q"))
		})
	}

	t.Run("Allow on aws:RequestTag grants nothing", func(t *testing.T) {
		docs := []iamdomain.PolicyDocument{{Version: "2012-10-17", Statement: []iamdomain.Statement{
			conditioned("Allow", "StringEquals", "aws:RequestTag/team", "a", "sqs:CreateQueue", "sqs:TagQueue"),
		}}}
		a := newSQSApp(t, docs, true)
		got := readWireError(t, a.raw(t, createTarget, []byte(`{"QueueName":"q","tags":{"team":"a"}}`)))
		assert.Equal(t, 403, got.status)
		assert.False(t, a.exists(t, "q"))
	})
}
