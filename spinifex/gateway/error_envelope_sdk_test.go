// Drives the real AWS SDK v2 clients against the gateway's error envelopes and
// asserts they decode them into their modelled errors.

package gateway

import (
	"bytes"
	"context"
	"errors"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/reqctx"
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/aws/smithy-go"
	"github.com/mulgadc/bluebottle/pkg/sigv4"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// registerTestCodes adds codes only the SDK fixtures raise, as the Lambda and
// SQS domains would, and removes them afterwards.
func registerTestCodes(t *testing.T, codes map[string]awserrors.ErrorMessage) {
	t.Helper()
	maps.Copy(awserrors.ErrorLookup, codes)
	t.Cleanup(func() {
		for code := range codes {
			delete(awserrors.ErrorLookup, code)
		}
	})
}

func sdkConfig(url string) aws.Config {
	return aws.Config{
		Region:           gwfix.Region,
		Credentials:      credentials.NewStaticCredentialsProvider(gwfix.AccessKey, gwfix.SecretKey, ""),
		RetryMaxAttempts: 1,
		BaseEndpoint:     aws.String(url),
	}
}

// sdkApp serves fixture sqs (JSON 1.0) and lambda (REST-JSON) registrations
// through the full gateway pipeline; their dispatchers fail with dispatchErr.
func sdkApp(t *testing.T, dispatchErr error) aws.Config {
	t.Helper()
	c := &dispatchCapture{dispatchErr: dispatchErr}
	_, app := dispatchSeamApp(t, allowAllECRDocs(),
		envelopeRegistration("sqs", dispatch.ErrorEnvelopeJSON10Query, c, func(r *dispatch.Registration) {
			r.QueryErrorCodes = map[string]string{"QueueDoesNotExist": "AWS.SimpleQueueService.NonExistentQueue"}
			r.ErrorNamespace = "com.amazonaws.sqs"
		}),
		envelopeRegistration("lambda", dispatch.ErrorEnvelopeRESTJSON, c, nil))
	srv := httptest.NewServer(app)
	t.Cleanup(srv.Close)
	return sdkConfig(srv.URL)
}

// throttleApp serves the gateway's throttle response for the signed service.
func throttleApp(t *testing.T) aws.Config {
	t.Helper()
	c := &dispatchCapture{}
	b := dispatch.NewBuilder()
	require.NoError(t, b.Register(envelopeRegistration("sqs", dispatch.ErrorEnvelopeJSON10Query, c,
		func(r *dispatch.Registration) { r.ErrorNamespace = "com.amazonaws.sqs" })))
	require.NoError(t, b.Register(envelopeRegistration("lambda", dispatch.ErrorEnvelopeRESTJSON, c, nil)))
	gw := &GatewayConfig{DisableLogging: true, Services: b.Build()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		svc := credentialScopeService(r)
		gw.writeThrottleError(w, r.WithContext(reqctx.WithService(r.Context(), svc)))
	}))
	t.Cleanup(srv.Close)
	return sdkConfig(srv.URL)
}

func TestSDKv2_SQSDecodesJSON10QueryErrors(t *testing.T) {
	registerTestCodes(t, map[string]awserrors.ErrorMessage{
		"QueueDoesNotExist": {HTTPCode: 400, Message: "The specified queue does not exist."},
	})
	cfg := sdkApp(t, awserrors.Errorf("QueueDoesNotExist", "no such queue"))

	_, err := sqs.NewFromConfig(cfg).GetQueueUrl(context.Background(), &sqs.GetQueueUrlInput{QueueName: aws.String("q")})
	require.Error(t, err)

	var modelled *sqstypes.QueueDoesNotExist
	require.ErrorAs(t, err, &modelled, "the SDK must map __type to its modelled error: %v", err)
	assert.Equal(t, "no such queue", aws.ToString(modelled.Message))

	var apiErr smithy.APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, "AWS.SimpleQueueService.NonExistentQueue", apiErr.ErrorCode(), "awsQueryCompatible clients report the legacy code")
	assert.Equal(t, smithy.FaultClient, apiErr.ErrorFault())
}

func TestSDKv2_SQSDecodesRequestThrottled(t *testing.T) {
	_, err := sqs.NewFromConfig(throttleApp(t)).GetQueueUrl(context.Background(), &sqs.GetQueueUrlInput{QueueName: aws.String("q")})

	var throttled *sqstypes.RequestThrottled
	require.ErrorAs(t, err, &throttled, "%v", err)
	assert.Equal(t, "Rate exceeded.", aws.ToString(throttled.Message))
}

func TestSDKv2_LambdaDecodesThrottleWithReason(t *testing.T) {
	registerTestCodes(t, map[string]awserrors.ErrorMessage{
		"TooManyRequestsException": {HTTPCode: 429, Message: "Rate Exceeded."},
	})
	cfg := sdkApp(t, awserrors.WithFields(
		awserrors.Errorf("TooManyRequestsException", "Rate Exceeded."),
		map[string]string{"Reason": "ReservedFunctionConcurrentInvocationLimitExceeded"}))

	_, err := lambda.NewFromConfig(cfg).GetFunction(context.Background(), &lambda.GetFunctionInput{FunctionName: aws.String("f")})
	var throttled *lambdatypes.TooManyRequestsException
	require.ErrorAs(t, err, &throttled, "%v", err)
	assert.Equal(t, lambdatypes.ThrottleReasonReservedFunctionConcurrentInvocationLimitExceeded, throttled.Reason)
	assert.Equal(t, "Rate Exceeded.", aws.ToString(throttled.Message))
}

func TestSDKv2_LambdaDecodesGatewayThrottle(t *testing.T) {
	_, err := lambda.NewFromConfig(throttleApp(t)).GetFunction(context.Background(), &lambda.GetFunctionInput{FunctionName: aws.String("f")})

	var throttled *lambdatypes.TooManyRequestsException
	require.ErrorAs(t, err, &throttled, "%v", err)
	assert.Equal(t, lambdatypes.ThrottleReason("CallerRateLimitExceeded"), throttled.Reason)
}

func TestSDKv2_LambdaDecodesServiceException(t *testing.T) {
	cfg := sdkApp(t, errors.New("not a registered code"))

	_, err := lambda.NewFromConfig(cfg).GetFunction(context.Background(), &lambda.GetFunctionInput{FunctionName: aws.String("f")})
	var svcErr *lambdatypes.ServiceException
	require.ErrorAs(t, err, &svcErr, "%v", err)
}

func TestSDKv2_LambdaOversizeBodyDecodesRequestEntityTooLarge(t *testing.T) {
	cfg := sdkApp(t, nil)
	oversized := bytes.Repeat([]byte("x"), int(sigv4.MaxPayloadLen)+1)

	_, err := lambda.NewFromConfig(cfg).Invoke(context.Background(), &lambda.InvokeInput{FunctionName: aws.String("f"), Payload: oversized})
	require.Error(t, err)

	// The Go SDK models this error as RequestTooLargeException, so AWS's own
	// RequestEntityTooLargeException surfaces as a generic API error.
	var apiErr smithy.APIError
	require.ErrorAs(t, err, &apiErr, "%v", err)
	assert.Equal(t, "RequestEntityTooLargeException", apiErr.ErrorCode())
}
