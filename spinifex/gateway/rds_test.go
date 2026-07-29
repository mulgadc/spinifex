package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupRDSRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	ctx := context.WithValue(req.Context(), ctxService, "rds")
	ctx = context.WithValue(ctx, ctxAccountID, "123456789012")
	return req.WithContext(ctx)
}

func TestRDSRequest_MissingAction(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	err := gw.RDS_Request(httptest.NewRecorder(), setupRDSRequest(""))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingAction, err.Error())
}

func TestRDSRequest_UnknownAction(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	err := gw.RDS_Request(httptest.NewRecorder(), setupRDSRequest("Action=FakeAction"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAction, err.Error())
}

// A malformed percent-encoding is a client error, not an internal one.
func TestRDSRequest_MalformedQueryString(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	err := gw.RDS_Request(httptest.NewRecorder(), setupRDSRequest("Action=%zz"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMalformedQueryString, err.Error())
}

// A known action still needs a NATS connection to reach the control plane, so
// the disconnected case must fail rather than answer from the gateway alone.
func TestRDSRequest_NoNATSConn(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	err := gw.RDS_Request(httptest.NewRecorder(), setupRDSRequest("Action=DescribeDBInstances"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

// RDS errors must use the IAM-style <ErrorResponse> envelope: the aws-sdk-go
// query unmarshaler rejects the EC2 <Response><Errors> shape for this service.
func TestRDSErrorHandler_UsesIAMEnvelope(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	w := httptest.NewRecorder()
	gw.ErrorHandler(w, setupRDSRequest("Action=CreateDBInstance"), errNotImplementedForTest)

	body := w.Body.String()
	assert.Contains(t, body, "<ErrorResponse>")
	assert.Contains(t, body, "<Code>"+awserrors.ErrorNotImplemented+"</Code>")
}

var errNotImplementedForTest = errors.New(awserrors.ErrorNotImplemented)
