package awsapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// bodySender answers each send with the error keyed by its body.
type bodySender struct {
	errs map[string]error
	sent []message.SendInput
}

func (s *bodySender) Send(_ context.Context, in message.SendInput) (message.SendResult, error) {
	s.sent = append(s.sent, in)
	if err := s.errs[string(in.Body)]; err != nil {
		return message.SendResult{}, err
	}
	return message.SendResult{MessageID: "id-" + string(in.Body), MD5OfBody: "md5-" + string(in.Body)}, nil
}

// pinQueues resolves every name to one queue, or fails with err; alter,
// when set, corrupts the record it returns.
type pinQueues struct {
	countingQueues

	err   error
	alter func(*queue.Record)
}

func (q *pinQueues) Resolve(_ context.Context, account, region, name string) (queue.Record, error) {
	q.calls++
	if q.err != nil {
		return queue.Record{}, q.err
	}
	r := queue.Record{QueueID: "qid-" + name, Account: account, Region: region, Name: name, State: queue.StateActive}
	if q.alter != nil {
		q.alter(&r)
	}
	return r, nil
}

func callSendBatch(t *testing.T, msgs awsapi.Messages, deny bool, body string) (*httptest.ResponseRecorder, *batchCall, error) {
	t.Helper()
	return callSendBatchOn(t, &pinQueues{}, msgs, deny, body)
}

func callSendBatchOn(t *testing.T, qs awsapi.Queues, msgs awsapi.Messages, deny bool, body string) (*httptest.ResponseRecorder, *batchCall, error) {
	t.Helper()
	reg := awsapi.NewRegistration(awsapi.Deps{Queues: qs, Messages: msgs, Endpoint: testEndpoint})
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	r.Header.Set("X-Amz-Target", awsapi.TargetPrefix+awsapi.OpSendMessageBatch)
	w := httptest.NewRecorder()
	c := &batchCall{}
	err := reg.Dispatch(w, dispatch.Invocation{
		Request: r, AccountID: testAccount, Region: testRegion, Service: awsapi.ServiceName,
		Authorize: func(_ string, action string, resources []string, _ iampolicy.ConditionKeys) error {
			c.actions = append(c.actions, action)
			c.resources = append(c.resources, resources...)
			if deny {
				return errors.New(awserrors.ErrorAccessDeniedException)
			}
			return nil
		},
		Actor: func() (dispatch.Actor, error) { return dispatch.Actor{UserID: "AIDAALICE"}, nil },
	})
	return w, c, err
}

func sendBatchReq(entries string) string {
	return `{"QueueUrl":"` + testQueueURL + `","Entries":[` + entries + `]}`
}

func TestSendMessageBatch_EntryOutcomes(t *testing.T) {
	msgs := &bodySender{errs: map[string]error{
		"uncertain": fmt.Errorf("%w: append: lost", message.ErrUnavailable),
		"both":      errors.Join(message.ErrInvalidParameter, message.ErrUnavailable),
		"gone":      message.ErrQueueDoesNotExist,
		"toolong":   fmt.Errorf("%w: message must be shorter than 1025 bytes", message.ErrInvalidParameter),
		"odd":       errors.New("boom"),
	}}
	w, c, err := callSendBatch(t, msgs, false, sendBatchReq(
		`{"Id":"a","MessageBody":"ok","DelaySeconds":5},{"Id":"b","MessageBody":"uncertain"},{"Id":"c","MessageBody":"both"},`+
			`{"Id":"d","MessageBody":"gone"},{"Id":"e","MessageBody":"toolong"},{"Id":"f","MessageBody":"odd"},{"Id":"g","MessageBody":"ok2"}`))
	require.NoError(t, err)
	assert.Equal(t, []string{"SendMessage"}, c.actions, "authorized once, as SendMessage")
	assert.Equal(t, []string{"arn:aws:sqs:" + testRegion + ":" + testAccount + ":q"}, c.resources)
	require.Len(t, msgs.sent, 7, "every entry is tried")
	first := msgs.sent[0]
	assert.Equal(t, []any{testAccount, testRegion, "q", "AIDAALICE", "ok", 5}, []any{first.Account, first.Region, first.Queue, first.Sender, string(first.Body), *first.DelaySeconds})
	assert.Nil(t, msgs.sent[1].DelaySeconds, "an absent delay leaves the queue's default")

	var out struct {
		Successful []map[string]any `json:"Successful"`
		Failed     []map[string]any `json:"Failed"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, []map[string]any{
		{"Id": "a", "MessageId": "id-ok", "MD5OfMessageBody": "md5-ok"},
		{"Id": "g", "MessageId": "id-ok2", "MD5OfMessageBody": "md5-ok2"},
	}, out.Successful)
	retry := "The send could not be confirmed; a retry may store this message twice."
	assert.Equal(t, []map[string]any{
		{"Id": "b", "SenderFault": false, "Code": "ServiceUnavailable", "Message": retry},
		{"Id": "c", "SenderFault": false, "Code": "ServiceUnavailable", "Message": retry},
		{"Id": "d", "SenderFault": true, "Code": "AWS.SimpleQueueService.NonExistentQueue", "Message": "The specified queue doesn't exist."},
		{"Id": "e", "SenderFault": true, "Code": "InvalidParameterValue", "Message": "message: invalid parameter value: message must be shorter than 1025 bytes"},
		{"Id": "f", "SenderFault": false, "Code": "InternalFailure", "Message": "The send failed."},
	}, out.Failed)
}

// The queue is resolved once after authorization; a name that resolves to
// nothing fails the request before any append.
func TestSendMessageBatch_QueueResolvedOnceBeforeAnyAppend(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"missing":     {queue.ErrQueueDoesNotExist, awserrors.ErrorSQSQueueDoesNotExist},
		"unavailable": {queue.ErrUnavailable, awserrors.ErrorServiceUnavailableException},
	} {
		t.Run(name, func(t *testing.T) {
			msgs := &bodySender{}
			_, c, err := callSendBatchOn(t, &pinQueues{err: tc.err}, msgs, false, sendBatchReq(`{"Id":"x","MessageBody":"x"},{"Id":"y","MessageBody":"y"}`))
			assert.Equal(t, tc.want, code(t, err))
			assert.Len(t, c.actions, 1, "authorized before the queue is looked up")
			assert.Empty(t, msgs.sent)
		})
	}

	qs := &pinQueues{}
	msgs := &bodySender{}
	_, _, err := callSendBatchOn(t, qs, msgs, false, sendBatchReq(`{"Id":"x","MessageBody":"x"},{"Id":"y","MessageBody":"y"}`))
	require.NoError(t, err)
	assert.Equal(t, 1, qs.calls)
	require.Len(t, msgs.sent, 2)
	for _, in := range msgs.sent {
		assert.Equal(t, message.QueueID("qid-q"), in.QueueID, "every entry is pinned to the resolved queue")
	}
}
