// Tests that the gateway renders dispatcher, throttle, cluster-unavailable and
// oversize-body errors through the envelope a registration declares.

package pipeline

import (
	"bytes"
	"errors"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/reqctx"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/mulgadc/bluebottle/pkg/sigv4"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/envelope"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	restJSONService  = "testrestjson"
	json10Service    = "testjson10"
	json11Service    = "testjson11"
	xmlRegisteredSvc = "testxmlreg"
)

func envelopeRegistration(service string, env dispatch.ErrorEnvelope, c *dispatchCapture, mod func(*dispatch.Registration)) dispatch.Registration {
	reg := dispatch.Registration{
		Service:       service,
		ResolveAction: func(*http.Request) string { return fakeAction },
		Dispatch:      c.dispatcher(),
		Errors:        env,
		Inventory:     dispatch.Inventory{Registered: []string{fakeAction}},
	}
	if mod != nil {
		mod(&reg)
	}
	return reg
}

func bodyString(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(b)
}

func throttleResponse(t *testing.T, regs ...dispatch.Registration) func(svc string) *http.Response {
	t.Helper()
	b := dispatch.NewBuilder()
	for _, reg := range regs {
		require.NoError(t, b.Register(reg))
	}
	gw := &Config{DisableLogging: true, Services: b.Build()}
	return func(svc string) *http.Response {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req = req.WithContext(reqctx.WithService(req.Context(), svc))
		w := httptest.NewRecorder()
		gw.writeThrottleError(w, req)
		return w.Result()
	}
}

func TestErrorEnvelope_Throttle(t *testing.T) {
	c := &dispatchCapture{}
	throttle := throttleResponse(t,
		envelopeRegistration(restJSONService, dispatch.ErrorEnvelopeRESTJSON, c, nil),
		envelopeRegistration(json10Service, dispatch.ErrorEnvelopeJSON10Query, c, nil),
		envelopeRegistration(json11Service, dispatch.ErrorEnvelopeJSON, c, nil),
		envelopeRegistration(xmlRegisteredSvc, dispatch.ErrorEnvelopeXML, c, nil),
	)

	namespaced := throttleResponse(t, envelopeRegistration(json10Service, dispatch.ErrorEnvelopeJSON10Query, c,
		func(r *dispatch.Registration) { r.ErrorNamespace = "com.amazonaws.sqs" }))

	t.Run("REST-JSON registration gets TooManyRequestsException with Reason", func(t *testing.T) {
		resp := throttle(restJSONService)
		assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
		assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
		assert.Equal(t, "TooManyRequestsException", resp.Header.Get("X-Amzn-Errortype"))
		assert.Empty(t, resp.Header.Get("Retry-After"))
		assert.JSONEq(t, `{"Reason":"CallerRateLimitExceeded","Type":"User","message":"Rate Exceeded."}`, bodyString(t, resp))
	})

	t.Run("JSON 1.0 registration gets RequestThrottled with the query header", func(t *testing.T) {
		resp := throttle(json10Service)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Equal(t, "application/x-amz-json-1.0", resp.Header.Get("Content-Type"))
		assert.Equal(t, "RequestThrottled;Sender", resp.Header.Get("X-Amzn-Query-Error"))
		assert.JSONEq(t, `{"__type":"RequestThrottled","message":"Rate exceeded."}`, bodyString(t, resp))
	})

	t.Run("JSON 1.0 namespaced throttle", func(t *testing.T) {
		resp := namespaced(json10Service)
		assert.Equal(t, "RequestThrottled;Sender", resp.Header.Get("X-Amzn-Query-Error"))
		assert.JSONEq(t, `{"__type":"com.amazonaws.sqs#RequestThrottled","message":"Rate exceeded."}`, bodyString(t, resp))
	})

	t.Run("JSON 1.1 registration keeps its current throttle", func(t *testing.T) {
		resp := throttle(json11Service)
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		assert.Equal(t, "application/x-amz-json-1.1", resp.Header.Get("Content-Type"))
		assert.Equal(t, "RequestLimitExceededException", resp.Header.Get("X-Amzn-Errortype"))
		assert.Contains(t, bodyString(t, resp), `"__type":"RequestLimitExceededException"`)
	})

	t.Run("XML registration keeps its current throttle", func(t *testing.T) {
		resp := throttle(xmlRegisteredSvc)
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		assert.Equal(t, "application/xml", resp.Header.Get("Content-Type"))
		assert.Contains(t, bodyString(t, resp), "<Code>RequestLimitExceeded</Code>")
	})
}

func TestErrorEnvelope_ThrottleOverride(t *testing.T) {
	c := &dispatchCapture{}
	throttle := throttleResponse(t, envelopeRegistration(restJSONService, dispatch.ErrorEnvelopeRESTJSON, c,
		func(r *dispatch.Registration) {
			r.Throttle = &dispatch.ErrorSpec{
				Code: "TooManyRequestsException", Message: "Slow down.", Status: http.StatusTooManyRequests,
				Fields: map[string]string{"Reason": "FunctionInvocationRateLimitExceeded"},
			}
		}))
	resp := throttle(restJSONService)
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	assert.JSONEq(t, `{"Reason":"FunctionInvocationRateLimitExceeded","Type":"User","message":"Slow down."}`, bodyString(t, resp))
}

func TestErrorEnvelope_DispatcherError(t *testing.T) {
	notFound := awserrors.WithFields(
		awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "bad value"),
		map[string]string{"Reason": "Because"})

	t.Run("REST-JSON uses the code as the type and carries service fields", func(t *testing.T) {
		c := &dispatchCapture{dispatchErr: notFound}
		_, app := dispatchSeamApp(t, allowAllECRDocs(), envelopeRegistration(restJSONService, dispatch.ErrorEnvelopeRESTJSON, c, nil))
		resp := doRequest(app, signDispatchRequest(t, restJSONService, []byte("{}")))

		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
		assert.Equal(t, "InvalidParameterValue", resp.Header.Get("X-Amzn-Errortype"))
		assert.Equal(t, `{"Reason":"Because","Type":"User","message":"bad value"}`, bodyString(t, resp))
	})

	t.Run("REST-JSON server fault is Type Service with its own status", func(t *testing.T) {
		c := &dispatchCapture{dispatchErr: awserrors.Errorf(awserrors.ErrorInternalError, "boom")}
		_, app := dispatchSeamApp(t, allowAllECRDocs(), envelopeRegistration(restJSONService, dispatch.ErrorEnvelopeRESTJSON, c, nil))
		resp := doRequest(app, signDispatchRequest(t, restJSONService, []byte("{}")))

		assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
		assert.Equal(t, "ServiceException", resp.Header.Get("X-Amzn-Errortype"))
		assert.JSONEq(t, `{"Type":"Service","message":"boom"}`, bodyString(t, resp))
	})

	t.Run("JSON 1.0 sends __type unsuffixed and maps the query code", func(t *testing.T) {
		c := &dispatchCapture{dispatchErr: awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "bad value")}
		_, app := dispatchSeamApp(t, allowAllECRDocs(), envelopeRegistration(json10Service, dispatch.ErrorEnvelopeJSON10Query, c,
			func(r *dispatch.Registration) {
				r.QueryErrorCodes = map[string]string{awserrors.ErrorInvalidParameterValue: "AWS.Simple.InvalidParameterValue"}
			}))
		resp := doRequest(app, signDispatchRequest(t, json10Service, []byte("{}")))

		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Equal(t, "application/x-amz-json-1.0", resp.Header.Get("Content-Type"))
		assert.Equal(t, "AWS.Simple.InvalidParameterValue;Sender", resp.Header.Get("X-Amzn-Query-Error"))
		assert.Empty(t, resp.Header.Get("X-Amzn-Errortype"))
		assert.Equal(t, `{"__type":"InvalidParameterValue","message":"bad value"}`, bodyString(t, resp))
	})

	t.Run("JSON 1.0 namespaces __type but not the query header", func(t *testing.T) {
		c := &dispatchCapture{dispatchErr: awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "bad value")}
		_, app := dispatchSeamApp(t, allowAllECRDocs(), envelopeRegistration(json10Service, dispatch.ErrorEnvelopeJSON10Query, c,
			func(r *dispatch.Registration) { r.ErrorNamespace = "com.amazonaws.sqs" }))
		resp := doRequest(app, signDispatchRequest(t, json10Service, []byte("{}")))

		assert.Equal(t, "InvalidParameterValue;Sender", resp.Header.Get("X-Amzn-Query-Error"))
		assert.Equal(t, `{"__type":"com.amazonaws.sqs#InvalidParameterValue","message":"bad value"}`, bodyString(t, resp))
	})

	t.Run("JSON 1.0 per-code namespace and query code for a framework error", func(t *testing.T) {
		c := &dispatchCapture{dispatchErr: awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "bad value")}
		_, app := dispatchSeamApp(t, allowAllECRDocs(), envelopeRegistration(json10Service, dispatch.ErrorEnvelopeJSON10Query, c,
			func(r *dispatch.Registration) {
				r.ErrorNamespace = "com.amazonaws.sqs"
				r.ErrorNamespaces = map[string]string{awserrors.ErrorInvalidParameterValue: envelope.CoralNamespace}
			}))
		resp := doRequest(app, signDispatchRequest(t, json10Service, []byte("{}")))
		assert.Equal(t, `{"__type":"com.amazon.coral.service#InvalidParameterValue","message":"bad value"}`, bodyString(t, resp))
	})

	t.Run("JSON 1.0 unmapped server fault is Receiver with InternalFailure", func(t *testing.T) {
		c := &dispatchCapture{dispatchErr: awserrors.Errorf(awserrors.ErrorInternalError, "boom")}
		_, app := dispatchSeamApp(t, allowAllECRDocs(), envelopeRegistration(json10Service, dispatch.ErrorEnvelopeJSON10Query, c, nil))
		resp := doRequest(app, signDispatchRequest(t, json10Service, []byte("{}")))

		assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
		assert.Equal(t, "InternalFailure;Receiver", resp.Header.Get("X-Amzn-Query-Error"))
		assert.Contains(t, bodyString(t, resp), `"__type":"InternalFailure"`)
	})

	t.Run("unknown code takes the registration's internal error code", func(t *testing.T) {
		c := &dispatchCapture{dispatchErr: errors.New("not a registered code")}
		_, app := dispatchSeamApp(t, allowAllECRDocs(), envelopeRegistration(restJSONService, dispatch.ErrorEnvelopeRESTJSON, c,
			func(r *dispatch.Registration) { r.InternalErrorCode = "CustomInternal" }))
		resp := doRequest(app, signDispatchRequest(t, restJSONService, []byte("{}")))

		assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
		assert.Equal(t, "CustomInternal", resp.Header.Get("X-Amzn-Errortype"))
	})

	t.Run("JSON 1.1 and XML keep InternalError", func(t *testing.T) {
		c := &dispatchCapture{dispatchErr: errors.New("not a registered code")}
		_, app := dispatchSeamApp(t, allowAllECRDocs(), envelopeRegistration(json11Service, dispatch.ErrorEnvelopeJSON, c, nil))
		resp := doRequest(app, signDispatchRequest(t, json11Service, []byte("{}")))
		assert.Equal(t, "InternalErrorException", resp.Header.Get("X-Amzn-Errortype"))
	})

	t.Run("JSON 1.1 still appends Exception", func(t *testing.T) {
		c := &dispatchCapture{dispatchErr: awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "bad value")}
		_, app := dispatchSeamApp(t, allowAllECRDocs(), envelopeRegistration(json11Service, dispatch.ErrorEnvelopeJSON, c, nil))
		resp := doRequest(app, signDispatchRequest(t, json11Service, []byte("{}")))

		assert.Equal(t, "application/x-amz-json-1.1", resp.Header.Get("Content-Type"))
		assert.Equal(t, "InvalidParameterValueException", resp.Header.Get("X-Amzn-Errortype"))
		assert.Equal(t, `{"__type":"InvalidParameterValueException","message":"bad value"}`, bodyString(t, resp))
	})
}

func TestErrorEnvelope_ClusterUnavailable(t *testing.T) {
	c := &dispatchCapture{}
	b := dispatch.NewBuilder()
	require.NoError(t, b.Register(envelopeRegistration(restJSONService, dispatch.ErrorEnvelopeRESTJSON, c, nil)))
	require.NoError(t, b.Register(envelopeRegistration(json10Service, dispatch.ErrorEnvelopeJSON10Query, c, nil)))
	gw := withVerifier(gwfix.WithDeps(&Config{DisableLogging: true, Services: b.Build()}, gwfix.Deps{IAM: allowAllIAMService()}))

	call := func(svc string) *http.Response {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req = req.WithContext(reqctx.WithService(req.Context(), svc))
		w := httptest.NewRecorder()
		gw.Request(w, req)
		return w.Result()
	}

	resp := call(restJSONService)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	assert.Equal(t, "ServiceUnavailableException", resp.Header.Get("X-Amzn-Errortype"))
	assert.JSONEq(t, `{"Type":"Service","message":"`+clusterUnavailableMsg+`"}`, bodyString(t, resp))

	resp = call(json10Service)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Equal(t, "application/x-amz-json-1.0", resp.Header.Get("Content-Type"))
	assert.Equal(t, "ServiceUnavailable;Receiver", resp.Header.Get("X-Amzn-Query-Error"))
	assert.JSONEq(t, `{"__type":"com.amazon.coral.service#ServiceUnavailableException","message":"`+clusterUnavailableMsg+`"}`, bodyString(t, resp))
}

func TestErrorEnvelope_OversizeBody(t *testing.T) {
	oversized := bytes.Repeat([]byte("x"), int(sigv4.MaxPayloadLen)+1)
	send := func(t *testing.T, app http.Handler, service string) *http.Response {
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(oversized))
		req.Host = "localhost:9999"
		gwfix.SignRequestAs(t, req, oversized, gwfix.AccessKey, gwfix.SecretKey, service, time.Now().UTC())
		return doRequest(app, req)
	}
	c := &dispatchCapture{}
	_, app := dispatchSeamApp(t, allowAllECRDocs(),
		envelopeRegistration(restJSONService, dispatch.ErrorEnvelopeRESTJSON, c, nil),
		envelopeRegistration(json10Service, dispatch.ErrorEnvelopeJSON10Query, c, nil))

	resp := send(t, app, restJSONService)
	assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	assert.Equal(t, "RequestEntityTooLargeException", resp.Header.Get("X-Amzn-Errortype"))
	assert.JSONEq(t, `{"Type":"User","message":"Request body exceeds the maximum allowed size."}`, bodyString(t, resp))

	resp = send(t, app, json10Service)
	assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
	assert.Equal(t, "text; charset=utf-8", resp.Header.Get("Content-Type"))
	assert.Empty(t, resp.Header.Get("X-Amzn-Query-Error"))
	assert.Equal(t, "HTTP content length exceeded "+strconv.Itoa(int(sigv4.MaxPayloadLen))+" bytes.", bodyString(t, resp))
}

func TestCredentialScopeService(t *testing.T) {
	for name, tc := range map[string]struct {
		header, want string
	}{
		"scope":     {"AWS4-HMAC-SHA256 Credential=AK/20260101/us-east-1/lambda/aws4_request, SignedHeaders=host, Signature=x", "lambda"},
		"no header": {"", ""},
		"truncated": {"AWS4-HMAC-SHA256 Credential=AK/20260101/us-east-1", ""},
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			assert.Equal(t, tc.want, credentialScopeService(req))
		})
	}
}
