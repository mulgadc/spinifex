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

// handleDeleter answers each delete with the error keyed by its handle.
type handleDeleter struct {
	errs    map[string]error
	handles []string
}

func (d *handleDeleter) Delete(_ context.Context, in message.DeleteInput) error {
	d.handles = append(d.handles, in.ReceiptHandle)
	return d.errs[in.ReceiptHandle]
}

type batchCall struct {
	actions   []string
	resources []string
}

func callBatch(t *testing.T, del awsapi.Deletes, deny bool, body string) (*httptest.ResponseRecorder, *batchCall, error) {
	t.Helper()
	reg := awsapi.NewRegistration(awsapi.Deps{Queues: &countingQueues{}, Deletes: del, Endpoint: testEndpoint})
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	r.Header.Set("X-Amz-Target", awsapi.TargetPrefix+awsapi.OpDeleteMessageBatch)
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

func batchReq(entries string) string {
	return `{"QueueUrl":"` + testQueueURL + `","Entries":[` + entries + `]}`
}

type batchReply struct {
	Successful []map[string]any `json:"Successful"`
	Failed     []map[string]any `json:"Failed"`
}

func TestDeleteMessageBatch_EntryOutcomes(t *testing.T) {
	del := &handleDeleter{errs: map[string]error{
		"h-forged":    message.ErrReceiptInvalid,
		"h-uncertain": message.ErrUnavailable,
		"h-gone":      message.ErrQueueDoesNotExist,
		"h-odd":       errors.New("boom"),
	}}
	w, c, err := callBatch(t, del, false, batchReq(
		`{"Id":"a","ReceiptHandle":"h-ok"},{"Id":"b","ReceiptHandle":"h-forged"},{"Id":"c","ReceiptHandle":"h-uncertain"},`+
			`{"Id":"d","ReceiptHandle":"h-gone"},{"Id":"e","ReceiptHandle":"h-odd"},{"Id":"f","ReceiptHandle":"h-ok2"}`))
	require.NoError(t, err)
	assert.Equal(t, []string{"DeleteMessage"}, c.actions, "authorized once, as DeleteMessage")
	assert.Equal(t, []string{"arn:aws:sqs:" + testRegion + ":" + testAccount + ":q"}, c.resources)
	assert.Equal(t, []string{"h-ok", "h-forged", "h-uncertain", "h-gone", "h-odd", "h-ok2"}, del.handles, "every entry is tried in order")

	var out batchReply
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, []map[string]any{{"Id": "a"}, {"Id": "f"}}, out.Successful)
	assert.Equal(t, []map[string]any{
		{"Id": "b", "SenderFault": true, "Code": "ReceiptHandleIsInvalid", "Message": "The specified receipt handle isn't valid."},
		{"Id": "c", "SenderFault": false, "Code": "ServiceUnavailable", "Message": "The delete could not be confirmed; retry this entry."},
		{"Id": "d", "SenderFault": true, "Code": "AWS.SimpleQueueService.NonExistentQueue", "Message": "The specified queue doesn't exist."},
		{"Id": "e", "SenderFault": false, "Code": "InternalFailure", "Message": "The delete failed."},
	}, out.Failed)
	assert.NotContains(t, w.Body.String(), "h-", "no receipt handle is echoed")
}

// An unknown deletion outcome stays retryable even when the owner's error
// also carries a client-fault sentinel.
func TestDeleteMessageBatch_UncertainOutranksInvalidHandle(t *testing.T) {
	del := &handleDeleter{errs: map[string]error{
		"h-joined":  errors.Join(message.ErrReceiptInvalid, message.ErrUnavailable),
		"h-wrapped": fmt.Errorf("confirm: %w: %w", message.ErrUnavailable, message.ErrReceiptInvalid),
		"h-forged":  message.ErrReceiptInvalid,
	}}
	w, _, err := callBatch(t, del, false, batchReq(
		`{"Id":"joined","ReceiptHandle":"h-joined"},{"Id":"wrapped","ReceiptHandle":"h-wrapped"},{"Id":"forged","ReceiptHandle":"h-forged"}`))
	require.NoError(t, err)
	var out batchReply
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Empty(t, out.Successful)
	assert.Equal(t, []map[string]any{
		{"Id": "joined", "SenderFault": false, "Code": "ServiceUnavailable", "Message": "The delete could not be confirmed; retry this entry."},
		{"Id": "wrapped", "SenderFault": false, "Code": "ServiceUnavailable", "Message": "The delete could not be confirmed; retry this entry."},
		{"Id": "forged", "SenderFault": true, "Code": "ReceiptHandleIsInvalid", "Message": "The specified receipt handle isn't valid."},
	}, out.Failed)
}
