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
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// handleVisibility answers each change with the error keyed by its handle.
type handleVisibility struct {
	errs map[string]error
	got  []message.ChangeVisibilityInput
}

func (v *handleVisibility) ChangeVisibility(_ context.Context, in message.ChangeVisibilityInput) error {
	v.got = append(v.got, in)
	return v.errs[in.ReceiptHandle]
}

func callChangeBatch(t *testing.T, qs awsapi.Queues, vis awsapi.Visibility, deny bool, body string) (*httptest.ResponseRecorder, *batchCall, error) {
	t.Helper()
	reg := awsapi.NewRegistration(awsapi.Deps{Queues: qs, Visibility: vis, Endpoint: testEndpoint})
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	r.Header.Set("X-Amz-Target", awsapi.TargetPrefix+awsapi.OpChangeVisibilityBatch)
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

func TestChangeVisibilityBatch_EntryOutcomes(t *testing.T) {
	vis := &handleVisibility{errs: map[string]error{
		"h-forged":    message.ErrReceiptInvalid,
		"h-stale":     message.ErrNotInflight,
		"h-cap":       message.ErrVisibilityCap,
		"h-uncertain": fmt.Errorf("%w: outcome unknown", message.ErrUnavailable),
		"h-both":      errors.Join(message.ErrReceiptInvalid, message.ErrUnavailable),
		"h-gone":      message.ErrQueueDoesNotExist,
		"h-odd":       errors.New("boom"),
	}}
	w, c, err := callChangeBatch(t, &pinQueues{}, vis, false, batchReq(
		`{"Id":"a","ReceiptHandle":"h-ok","VisibilityTimeout":7},{"Id":"b","ReceiptHandle":"h-forged","VisibilityTimeout":0},`+
			`{"Id":"c","ReceiptHandle":"h-stale","VisibilityTimeout":0},{"Id":"d","ReceiptHandle":"h-cap","VisibilityTimeout":43200},`+
			`{"Id":"e","ReceiptHandle":"h-uncertain","VisibilityTimeout":0},{"Id":"f","ReceiptHandle":"h-both","VisibilityTimeout":0},`+
			`{"Id":"g","ReceiptHandle":"h-gone","VisibilityTimeout":0},{"Id":"h","ReceiptHandle":"h-odd","VisibilityTimeout":0},`+
			`{"Id":"i","ReceiptHandle":"h-ok","VisibilityTimeout":43201},{"Id":"j","ReceiptHandle":"h-ok","VisibilityTimeout":-1}`))
	require.NoError(t, err)
	assert.Equal(t, []string{"ChangeMessageVisibility"}, c.actions, "authorized once, as ChangeMessageVisibility")
	assert.Equal(t, []string{"arn:aws:sqs:" + testRegion + ":" + testAccount + ":q"}, c.resources)
	require.Len(t, vis.got, 8, "out-of-range timeouts never reach the owner")
	assert.Equal(t, message.ChangeVisibilityInput{Account: testAccount, Region: testRegion, Queue: "q", ReceiptHandle: "h-ok", VisibilityTimeout: 7}, vis.got[0])

	var out batchReply
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, []map[string]any{{"Id": "a"}}, out.Successful)
	retry := "The visibility change could not be confirmed; retry this entry."
	invalidTimeout := "VisibilityTimeout must be between 0 and 43200."
	assert.Equal(t, []map[string]any{
		{"Id": "b", "SenderFault": true, "Code": "ReceiptHandleIsInvalid", "Message": "The specified receipt handle isn't valid."},
		{"Id": "c", "SenderFault": true, "Code": "AWS.SimpleQueueService.MessageNotInflight", "Message": "The message referred to isn't in flight."},
		{"Id": "d", "SenderFault": true, "Code": "InvalidParameterValue", "Message": "VisibilityTimeout would extend past 12 hours from this receive."},
		{"Id": "e", "SenderFault": false, "Code": "ServiceUnavailable", "Message": retry},
		{"Id": "f", "SenderFault": false, "Code": "ServiceUnavailable", "Message": retry},
		{"Id": "g", "SenderFault": true, "Code": "AWS.SimpleQueueService.NonExistentQueue", "Message": "The specified queue doesn't exist."},
		{"Id": "h", "SenderFault": false, "Code": "InternalFailure", "Message": "The visibility change failed."},
		{"Id": "i", "SenderFault": true, "Code": "InvalidParameterValue", "Message": invalidTimeout},
		{"Id": "j", "SenderFault": true, "Code": "InvalidParameterValue", "Message": invalidTimeout},
	}, out.Failed)
	assert.NotContains(t, w.Body.String(), "h-", "no receipt handle is echoed")
}
