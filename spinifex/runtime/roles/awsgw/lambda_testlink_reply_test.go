//go:build lambdatestlink

package awsgw

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit/gwtest"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/awsapi"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/function"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/pipeline"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/reqctx"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/lambda/testlink"
	"github.com/stretchr/testify/require"
)

// storedFunction serves one Active function "f" whose code is stored.
type storedFunction struct{}

var _ function.Client = storedFunction{}

func (storedFunction) Get(_ context.Context, account, region, name string) (function.Record, uint64, error) {
	if name != "f" {
		return function.Record{}, 0, function.ErrNotFound
	}
	rec := invocation(nil).Function
	rec.AccountID, rec.Region, rec.State = account, region, function.StateActive
	rec.Code.State = function.CodeStored
	return rec, 1, nil
}

func (storedFunction) Create(context.Context, string, string, function.Record) (uint64, error) {
	return 0, errors.New("unused")
}

func (storedFunction) CodeStored(context.Context, string, string, string, uint64, string, int64) (function.Record, uint64, error) {
	return function.Record{}, 0, errors.New("unused")
}

func (storedFunction) RollbackCreate(context.Context, string, string, string, uint64) error {
	return errors.New("unused")
}

// invokeThrough sends one Invoke through the Lambda handler to the real
// test-link executor; fakeControl stands in for the daemon and answers with rep.
func invokeThrough(t *testing.T, rep testlink.Reply) (*httptest.ResponseRecorder, error) {
	t.Helper()
	n := &fakeControl{max: 1 << 20, reply: &rep}
	reg := awsapi.NewRegistration(awsapi.Deps{Meta: storedFunction{}, Exec: executor(n, nil, grantStore(t))})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/2015-03-31/functions/f/invocations", strings.NewReader(`{}`))
	err := gwtest.AllowAll().Dispatch(reg, awsapi.ServiceName, rec, req)
	require.Len(t, n.sent, 1, "the invocation reached the daemon")
	return rec, err
}

// The session's reply for an oversized function response reaches the caller
// as 200 with an Unhandled function error.
func TestLambdaTestLinkReply_OversizedResponseIsUnhandled(t *testing.T) {
	payload := `{"errorType":"Function.ResponseSizeTooLarge","errorMessage":"too large"}`
	rec, err := invokeThrough(t, testlink.Reply{Outcome: testlink.OutcomeDone, FunctionError: "Unhandled", Payload: []byte(payload)})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "Unhandled", rec.Header().Get("X-Amz-Function-Error"))
	require.JSONEq(t, payload, rec.Body.String())
}

// An unproven timeout claim is an unknown outcome, never a 200: the gateway's
// REST-JSON error handler writes it as a 500 ServiceException.
func TestLambdaTestLinkReply_UnprovenTimeoutIsUnknown(t *testing.T) {
	_, err := invokeThrough(t, testlink.Reply{Outcome: testlink.OutcomeUnknown, Error: testlink.ErrTimeoutUnproven.Error()})
	require.Error(t, err)

	b := dispatch.NewBuilder()
	require.NoError(t, b.Register(awsapi.NewRegistration(awsapi.Deps{})))
	gw := &pipeline.Config{Services: b.Build()}
	req := httptest.NewRequest(http.MethodPost, "/2015-03-31/functions/f/invocations", nil)
	req = req.WithContext(reqctx.WithService(req.Context(), awsapi.ServiceName))
	rec := httptest.NewRecorder()
	gw.ErrorHandler(rec, req, err)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Equal(t, "ServiceException", rec.Header().Get("X-Amzn-Errortype"))
	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.NotEmpty(t, body["message"])
	require.NotContains(t, rec.Body.String(), "Sandbox.Timedout")
}
