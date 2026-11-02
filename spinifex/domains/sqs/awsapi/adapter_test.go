package awsapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/awsapi"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/queue"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testAccount  = "123456789012"
	testRegion   = "ap-southeast-2"
	testEndpoint = "https://sqs.example:9999"
	testQueueURL = testEndpoint + "/" + testAccount + "/q"
)

// recordingMessages captures every send so a test can tell a refused request
// from one that reached the message owner.
type recordingMessages struct {
	sent []message.SendInput
	err  error
}

func (m *recordingMessages) Send(_ context.Context, in message.SendInput) (message.SendResult, error) {
	m.sent = append(m.sent, in)
	if m.err != nil {
		return message.SendResult{}, m.err
	}
	return message.SendResult{MessageID: "id-1", MD5OfBody: "md5"}, nil
}

// countingQueues fails the test's expectations by counting any owner call.
type countingQueues struct{ calls int }

func (q *countingQueues) Create(context.Context, queue.CreateRequest) (queue.Record, error) {
	q.calls++
	return queue.Record{}, queue.ErrUnavailable
}

func (q *countingQueues) Resolve(context.Context, string, string, string) (queue.Record, error) {
	q.calls++
	return queue.Record{}, queue.ErrUnavailable
}

func (q *countingQueues) List(context.Context, queue.ListRequest) (queue.ListResult, error) {
	q.calls++
	return queue.ListResult{}, queue.ErrUnavailable
}

func (q *countingQueues) SetAttributes(context.Context, string, string, string, map[string]string) (queue.Record, error) {
	q.calls++
	return queue.Record{}, queue.ErrUnavailable
}

func (q *countingQueues) Tag(context.Context, string, string, string, map[string]string) (queue.Record, error) {
	q.calls++
	return queue.Record{}, queue.ErrUnavailable
}

func (q *countingQueues) Untag(context.Context, string, string, string, []string) (queue.Record, error) {
	q.calls++
	return queue.Record{}, queue.ErrUnavailable
}

func (q *countingQueues) Tags(context.Context, string, string, string) (map[string]string, error) {
	q.calls++
	return nil, queue.ErrUnavailable
}

type harness struct {
	msgs       *recordingMessages
	queues     *countingQueues
	ops        *stubMessageOps
	authorized int
}

func newHarness() *harness {
	return &harness{msgs: &recordingMessages{}, queues: &countingQueues{}, ops: &stubMessageOps{}}
}

// call dispatches one operation as an authenticated caller who is allowed
// everything, returning the response recorder and the dispatch error.
func (h *harness) call(t *testing.T, op, body string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	reg := awsapi.NewRegistration(awsapi.Deps{
		Queues: h.queues, Messages: h.msgs, Receives: h.ops, Deletes: h.ops, Visibility: h.ops, Endpoint: testEndpoint,
	})
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	if op != "" {
		r.Header.Set("X-Amz-Target", awsapi.TargetPrefix+op)
	}
	w := httptest.NewRecorder()
	err := reg.Dispatch(w, dispatch.Invocation{
		Request: r, AccountID: testAccount, Region: testRegion, Service: awsapi.ServiceName,
		Authorize: func(string, string, []string, iampolicy.ConditionKeys) error {
			h.authorized++
			return nil
		},
		Actor: func() (dispatch.Actor, error) { return dispatch.Actor{UserID: "AIDAALICE"}, nil },
	})
	return w, err
}

func code(t *testing.T, err error) string {
	t.Helper()
	require.Error(t, err)
	c, ok := awserrors.ResolveErrorCode(err)
	require.True(t, ok, "unresolved error %v", err)
	return c
}

func sendBody(rawBody string) string {
	return `{"QueueUrl":"` + testQueueURL + `","MessageBody":` + rawBody + `}`
}

// Every operation reads exactly one JSON object; nothing but whitespace may
// follow it, and the owner is never called otherwise.
func TestDecode_StrictSingleObject(t *testing.T) {
	ops := []string{awsapi.OpCreateQueue, awsapi.OpGetQueueURL, awsapi.OpListQueues, awsapi.OpSendMessage}
	for _, op := range ops {
		for _, body := range []string{
			`{"QueueName":"q"}}`,
			`{"QueueName":"q"}]`,
			`{"QueueName":"q"}{}`,
			`{"QueueName":"q"} 1`,
			`null`,
			`[]`,
			`"q"`,
			`{"QueueName":`,
		} {
			h := newHarness()
			_, err := h.call(t, op, body)
			assert.Equal(t, awserrors.ErrorSerializationException, code(t, err), "%s %s", op, body)
			assert.Zero(t, h.queues.calls, "%s %s", op, body)
			assert.Empty(t, h.msgs.sent, "%s %s", op, body)
		}
	}

	h := newHarness()
	_, err := h.call(t, awsapi.OpSendMessage, " \n"+sendBody(`"x"`)+" \t\r\n")
	require.NoError(t, err, "surrounding whitespace is accepted")
	require.Len(t, h.msgs.sent, 1)
}

func TestSendMessage_QueueURLBoundary(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"http://sqs.example:9999/" + testAccount + "/q", awserrors.ErrorSQSInvalidAddress},
		{"https://sqs.example/" + testAccount + "/q", awserrors.ErrorSQSInvalidAddress},
		{"https://other.example:9999/" + testAccount + "/q", awserrors.ErrorSQSInvalidAddress},
		{"https://sqs.example:9999/210987654321/q", awserrors.ErrorSQSInvalidAddress},
		{testQueueURL + "?x=1", awserrors.ErrorSQSInvalidAddress},
		{testQueueURL + "?", awserrors.ErrorSQSInvalidAddress},
		{testQueueURL + "#f", awserrors.ErrorSQSInvalidAddress},
		{"https://u@sqs.example:9999/" + testAccount + "/q", awserrors.ErrorSQSInvalidAddress},
		{testQueueURL + "/extra", awserrors.ErrorSQSInvalidAddress},
		{testEndpoint + "/" + testAccount, awserrors.ErrorSQSInvalidAddress},
		{"sqs.example:9999/" + testAccount + "/q", awserrors.ErrorSQSInvalidAddress},
		{testEndpoint + "/" + testAccount + "/bad.name", awserrors.ErrorSQSQueueDoesNotExist},
		{testEndpoint + "/" + testAccount + "/" + strings.Repeat("a", 81), awserrors.ErrorSQSQueueDoesNotExist},
	} {
		raw, want := tc.raw, tc.want
		h := newHarness()
		body := `{"QueueUrl":"` + raw + `","MessageBody":"x"}`
		_, err := h.call(t, awsapi.OpSendMessage, body)
		assert.Equal(t, want, code(t, err), raw)
		assert.Empty(t, h.msgs.sent, raw)
		assert.Zero(t, h.authorized, "%s: refused before authorizing", raw)
	}

	h := newHarness()
	_, err := h.call(t, awsapi.OpSendMessage, `{"QueueUrl":"HTTPS://SQS.EXAMPLE:9999/`+testAccount+`/q","MessageBody":"x"}`)
	require.NoError(t, err, "scheme and host compare case-insensitively")
	require.Len(t, h.msgs.sent, 1)
	assert.Equal(t, "q", h.msgs.sent[0].Queue)
}

// Unsupported members are refused when present at all, empty included.
func TestSendMessage_UnsupportedMembersByPresence(t *testing.T) {
	for _, member := range []string{
		`"MessageGroupId":""`,
		`"MessageGroupId":"g"`,
		`"MessageDeduplicationId":""`,
		`"MessageDeduplicationId":"d"`,
		`"MessageAttributes":{"a":{"DataType":"Number","StringValue":"1"}}`,
		`"MessageSystemAttributes":{"AWSTraceHeader":{"DataType":"String","StringValue":"v"}}`,
	} {
		h := newHarness()
		body := `{"QueueUrl":"` + testQueueURL + `","MessageBody":"x",` + member + `}`
		_, err := h.call(t, awsapi.OpSendMessage, body)
		assert.Equal(t, awserrors.ErrorUnsupportedOperation, code(t, err), member)
		assert.Empty(t, h.msgs.sent, member)
	}
}

func TestSendMessage_OwnerErrorsMapped(t *testing.T) {
	for ownerErr, want := range map[error]string{
		message.ErrQueueDoesNotExist: awserrors.ErrorSQSQueueDoesNotExist,
		message.ErrUnavailable:       awserrors.ErrorServiceUnavailableException,
		message.ErrInvalidContents:   awserrors.ErrorSQSInvalidMessageContents,
		message.ErrInvalidParameter:  awserrors.ErrorInvalidParameterValueException,
		errors.New("unexpected"):     awserrors.ErrorInternalFailure,
	} {
		h := newHarness()
		h.msgs.err = ownerErr
		w, err := h.call(t, awsapi.OpSendMessage, sendBody(`"x"`))
		assert.Equal(t, want, code(t, err), ownerErr.Error())
		assert.Empty(t, w.Body.String(), "%s: no success body", ownerErr)
	}
}
