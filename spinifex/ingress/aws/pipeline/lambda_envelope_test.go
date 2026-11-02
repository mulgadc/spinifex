package pipeline

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	lambdaawsapi "github.com/mulgadc/spinifex/spinifex/domains/lambda/awsapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A refused CreateFunction leaves the real ingress in the REST-JSON shape the
// Lambda SDK decodes: the code in X-Amzn-Errortype, Type and message in the body.
func TestLambdaCreateFunctionErrorUsesRESTJSONEnvelope(t *testing.T) {
	_, app := dispatchSeamApp(t, allowAllECRDocs(), lambdaawsapi.NewRegistration(lambdaawsapi.Deps{}))

	body := []byte(`{"FunctionName":"hello","TenancyConfig":{}}`)
	req := httptest.NewRequest(http.MethodPost, "/2015-03-31/functions", bytes.NewReader(body))
	req.Host = "localhost:9999"
	gwfix.SignRequestAs(t, req, body, gwfix.AccessKey, gwfix.SecretKey, lambdaawsapi.ServiceName, time.Now().UTC())
	resp := doRequest(app, req)

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	assert.Equal(t, "InvalidParameterValueException", resp.Header.Get("X-Amzn-Errortype"))
	assert.NotEmpty(t, resp.Header.Get("X-Amzn-Requestid"))
	env := decodeJSONError(t, resp)
	require.NotEmpty(t, env.Message)
	assert.Empty(t, env.Type, "REST-JSON carries the code in the header, not __type")
}
