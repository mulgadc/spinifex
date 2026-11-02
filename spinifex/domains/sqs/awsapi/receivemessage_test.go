package awsapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/awsapi"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubMessageOps answers receives with got and records each request.
type stubMessageOps struct {
	got      []message.Received
	err      error
	receives []message.ReceiveInput
	deletes  []message.DeleteInput
	changes  []message.ChangeVisibilityInput
}

func (s *stubMessageOps) ChangeVisibility(_ context.Context, in message.ChangeVisibilityInput) error {
	s.changes = append(s.changes, in)
	return s.err
}

func (s *stubMessageOps) Receive(_ context.Context, in message.ReceiveInput) ([]message.Received, error) {
	s.receives = append(s.receives, in)
	return s.got, s.err
}

func (s *stubMessageOps) Delete(_ context.Context, in message.DeleteInput) error {
	s.deletes = append(s.deletes, in)
	return s.err
}

func recvReq(rest string) string { return `{"QueueUrl":"` + testQueueURL + `"` + rest + `}` }

var sample = message.Received{
	MessageID: "m-1", ReceiptHandle: "h-1", Body: []byte("hi é"), MD5OfBody: "abc",
	SenderID: "AIDAALICE", SentAt: time.UnixMilli(1_700_000_000_123).UTC(), ReceiveCount: 3,
	FirstReceive: time.UnixMilli(1_700_000_005_456).UTC(),
}

type wireMessage struct {
	MessageID     string            `json:"MessageId"`
	ReceiptHandle string            `json:"ReceiptHandle"`
	MD5OfBody     string            `json:"MD5OfBody"`
	Body          string            `json:"Body"`
	Attributes    map[string]string `json:"Attributes"`
}

func received(t *testing.T, h *harness, body string) []wireMessage {
	t.Helper()
	w, err := h.call(t, awsapi.OpReceiveMessage, body)
	require.NoError(t, err)
	var out struct {
		Messages []wireMessage `json:"Messages"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	return out.Messages
}

func TestReceiveMessage_RendersMessageAndAskedAttributes(t *testing.T) {
	all := map[string]string{
		"SenderId": "AIDAALICE", "SentTimestamp": "1700000000123",
		"ApproximateReceiveCount": "3", "ApproximateFirstReceiveTimestamp": "1700000005456",
	}
	for name, tc := range map[string]struct {
		rest string
		want map[string]string
	}{
		"none asked":     {"", nil},
		"all":            {`,"MessageSystemAttributeNames":["All"]`, all},
		"legacy all":     {`,"AttributeNames":["All"]`, all},
		"one":            {`,"MessageSystemAttributeNames":["SentTimestamp"]`, map[string]string{"SentTimestamp": "1700000000123"}},
		"both spellings": {`,"AttributeNames":["SenderId"],"MessageSystemAttributeNames":["ApproximateReceiveCount"]`, map[string]string{"SenderId": "AIDAALICE", "ApproximateReceiveCount": "3"}},
		"never carried":  {`,"MessageSystemAttributeNames":["SequenceNumber","MessageGroupId","MessageDeduplicationId","AWSTraceHeader","DeadLetterQueueSourceArn"]`, nil},
		"first receive":  {`,"MessageSystemAttributeNames":["ApproximateFirstReceiveTimestamp"]`, map[string]string{"ApproximateFirstReceiveTimestamp": "1700000005456"}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness()
			h.ops.got = []message.Received{sample}
			got := received(t, h, recvReq(tc.rest))
			require.Len(t, got, 1)
			assert.Equal(t, wireMessage{MessageID: "m-1", ReceiptHandle: "h-1", MD5OfBody: "abc", Body: "hi é", Attributes: tc.want}, got[0])
		})
	}
}

// An outage that wraps an invalid handle is still an outage, never a claim
// that the handle is bad.
func TestMessageOps_OwnerErrorsMapped(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{message.ErrQueueDoesNotExist, awserrors.ErrorSQSQueueDoesNotExist},
		{message.ErrReceiptInvalid, awserrors.ErrorSQSReceiptHandleIsInvalid},
		{message.ErrNotInflight, awserrors.ErrorSQSMessageNotInflight},
		{message.ErrVisibilityCap, awserrors.ErrorInvalidParameterValueException},
		{fmt.Errorf("%w: %w", message.ErrUnavailable, message.ErrNotInflight), awserrors.ErrorServiceUnavailableException},
		{fmt.Errorf("%w: %w", message.ErrUnavailable, message.ErrReceiptInvalid), awserrors.ErrorServiceUnavailableException},
		{message.ErrUnavailable, awserrors.ErrorServiceUnavailableException},
		{message.ErrInvalidParameter, awserrors.ErrorInvalidParameterValueException},
		{message.ErrUnsupported, awserrors.ErrorUnsupportedOperation},
		{errors.New("unexpected"), awserrors.ErrorInternalFailure},
	} {
		for _, op := range []struct{ name, body string }{
			{awsapi.OpReceiveMessage, recvReq("")},
			{awsapi.OpDeleteMessage, `{"QueueUrl":"` + testQueueURL + `","ReceiptHandle":"h"}`},
			{awsapi.OpChangeVisibility, `{"QueueUrl":"` + testQueueURL + `","ReceiptHandle":"h","VisibilityTimeout":5}`},
		} {
			h := newHarness()
			h.ops.err = tc.err
			w, err := h.call(t, op.name, op.body)
			assert.Equal(t, tc.want, code(t, err), "%s: %v", op.name, tc.err)
			assert.Empty(t, w.Body.String())
		}
	}
}
