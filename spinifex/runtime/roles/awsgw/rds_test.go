package awsgw_test

import (
	"errors"
	"github.com/mulgadc/spinifex/spinifex/gateway"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/reqctx"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
)

const rdsTestAccountID = "123456789012"

func setupRDSRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	ctx := reqctx.WithService(req.Context(), "rds")
	ctx = withPrincipalField(ctx, fieldAccountID, rdsTestAccountID)
	return withTestIdentity(req.WithContext(ctx))
}

// RDS errors must use the IAM-style <ErrorResponse> envelope: the aws-sdk-go
// query unmarshaler rejects the EC2 <Response><Errors> shape for this service.
func TestRDSErrorHandler_UsesIAMEnvelope(t *testing.T) {
	gw := withVerifier(withRDS(&gateway.GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()}))
	w := httptest.NewRecorder()
	gw.ErrorHandler(w, setupRDSRequest("Action=CreateDBInstance"), errNotImplementedForTest)

	body := w.Body.String()
	assert.Contains(t, body, "<ErrorResponse>")
	assert.Contains(t, body, "<Code>"+awserrors.ErrorNotImplemented+"</Code>")
}

func TestRDSErrorHandler_UsesRDSUnsupportedActionWording(t *testing.T) {
	gw := withVerifier(withRDS(&gateway.GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()}))
	w := httptest.NewRecorder()
	gw.ErrorHandler(w, setupRDSRequest("Action=CreateDBCluster"),
		errors.New(awserrors.ErrorOperationNotSupported))

	body := w.Body.String()
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, body, "RDS")
	assert.Contains(t, body, "v1")
	assert.NotContains(t, body, "registry")
}

var errNotImplementedForTest = errors.New(awserrors.ErrorNotImplemented)
