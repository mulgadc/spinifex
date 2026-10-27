package envelope

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEC2Body_Structure(t *testing.T) {
	tests := []struct {
		name      string
		code      string
		message   string
		requestID string
	}{
		{
			name:      "standard error",
			code:      "InvalidParameterValue",
			message:   "The value supplied is not valid.",
			requestID: "req-12345",
		},
		{
			name:      "auth failure",
			code:      "AuthFailure",
			message:   "Credentials could not be validated.",
			requestID: "req-auth-001",
		},
		{
			name:      "empty fields",
			code:      "",
			message:   "",
			requestID: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			output := EC2Body(tc.code, tc.message, tc.requestID)
			require.NotNil(t, output)

			xmlStr := string(output)

			assert.True(t, strings.HasPrefix(xmlStr, xml.Header))
			assert.Contains(t, xmlStr, "<Code>"+tc.code+"</Code>")
			assert.Contains(t, xmlStr, "<RequestID>"+tc.requestID+"</RequestID>")

			// EC2 query API uses <Response>/<Errors>, not <ErrorResponse>; aws-sdk-go v1
			// rejects the latter with SerializationError.
			assert.Contains(t, xmlStr, "<Response>")
			assert.Contains(t, xmlStr, "</Response>")
			assert.Contains(t, xmlStr, "<Errors>")
			assert.Contains(t, xmlStr, "<Error>")
		})
	}
}

// xmlRootName returns the local name of body's root element, which is what
// distinguishes the S3, IAM and EC2 error envelopes from each other.
func xmlRootName(t *testing.T, body []byte) string {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(string(body)))
	for {
		tok, err := dec.Token()
		require.NoError(t, err)
		if se, ok := tok.(xml.StartElement); ok {
			return se.Name.Local
		}
	}
}

func TestS3Body_FlatEnvelope(t *testing.T) {
	output := S3Body("SignatureDoesNotMatch", "bad signature", "req-s3-1", "/bucket/key.txt")
	require.NotNil(t, output)

	assert.True(t, strings.HasPrefix(string(output), xml.Header))
	assert.Equal(t, "Error", xmlRootName(t, output))

	var parsed struct {
		Code      string `xml:"Code"`
		Message   string `xml:"Message"`
		Resource  string `xml:"Resource"`
		RequestID string `xml:"RequestId"`
	}
	require.NoError(t, xml.Unmarshal(output, &parsed))

	assert.Equal(t, "SignatureDoesNotMatch", parsed.Code)
	assert.Equal(t, "bad signature", parsed.Message)
	assert.Equal(t, "/bucket/key.txt", parsed.Resource)
	assert.Equal(t, "req-s3-1", parsed.RequestID)
}

func TestS3Body_OmitsEmptyResource(t *testing.T) {
	output := S3Body("AccessDenied", "denied", "req-s3-2", "")
	assert.NotContains(t, string(output), "<Resource>")
}

func TestEC2Body_ValidXML(t *testing.T) {
	output := EC2Body("TestCode", "Test message", "req-999")
	require.NotNil(t, output)

	xmlBody := strings.TrimPrefix(string(output), xml.Header)
	decoder := xml.NewDecoder(strings.NewReader(xmlBody))
	for {
		_, err := decoder.Token()
		if err != nil {
			assert.ErrorIs(t, err, io.EOF)
			break
		}
	}
}

func TestIAMBody_Structure(t *testing.T) {
	tests := []struct {
		name      string
		code      string
		message   string
		requestID string
	}{
		{
			name:      "entity not found",
			code:      "NoSuchEntity",
			message:   "The request was rejected because it referenced a resource entity that does not exist.",
			requestID: "req-iam-001",
		},
		{
			name:      "entity already exists",
			code:      "EntityAlreadyExists",
			message:   "The request was rejected because it attempted to create a resource that already exists.",
			requestID: "req-iam-002",
		},
		{
			name:      "empty fields",
			code:      "",
			message:   "",
			requestID: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			output := IAMBody(tc.code, tc.message, tc.requestID)
			require.NotNil(t, output)

			xmlStr := string(output)

			assert.True(t, strings.HasPrefix(xmlStr, xml.Header))
			assert.Contains(t, xmlStr, "<ErrorResponse>")
			assert.Contains(t, xmlStr, "</ErrorResponse>")
			assert.Contains(t, xmlStr, "<Type>Sender</Type>")
			assert.Contains(t, xmlStr, "<Code>"+tc.code+"</Code>")
			assert.Contains(t, xmlStr, "<RequestId>"+tc.requestID+"</RequestId>")
		})
	}
}

func TestIAMBody_ValidXML(t *testing.T) {
	output := IAMBody("NoSuchEntity", "Entity not found", "req-iam-999")
	require.NotNil(t, output)

	xmlBody := strings.TrimPrefix(string(output), xml.Header)
	decoder := xml.NewDecoder(strings.NewReader(xmlBody))
	for {
		_, err := decoder.Token()
		if err != nil {
			assert.ErrorIs(t, err, io.EOF)
			break
		}
	}
}

// TestEC2Body_SDKRoundTrip serves the EC2 error envelope from an httptest
// server, points an aws-sdk-go v1 EC2 client at it, and asserts the SDK
// surfaces the code via awserr.Error.Code() — not SerializationError.
// aws-sdk-go v1's ec2query handler rejects the IAM <ErrorResponse> envelope
// and discards the embedded code, so the EC2 <Response>/<Errors> shape is
// required.
func TestEC2Body_SDKRoundTrip(t *testing.T) {
	const wantCode = "InvalidInstanceType"
	const wantMessage = "The instance type 't2.micro' is not supported in this region."

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body := EC2Body(wantCode, wantMessage, "req-sdk-roundtrip")
		w.Header().Set("Content-Type", XMLContentType)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	sess, err := session.NewSession(&aws.Config{
		Region:      aws.String("us-east-1"),
		Endpoint:    aws.String(srv.URL),
		Credentials: credentials.NewStaticCredentials("AKIA-TEST", "secret", ""),
		DisableSSL:  aws.Bool(true),
		// Suppress the default retry loop — error responses are not retryable
		// here and waiting them out wastes test time.
		MaxRetries: aws.Int(0),
	})
	require.NoError(t, err)

	client := ec2.New(sess)
	_, err = client.RunInstances(&ec2.RunInstancesInput{
		ImageId:      aws.String("ami-test"),
		InstanceType: aws.String("t2.micro"),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
	})
	require.Error(t, err)

	var reqErr awserr.Error
	ok := errors.As(err, &reqErr)
	require.True(t, ok, "expected an awserr.Error, got %T: %v", err, err)
	assert.Equal(t, wantCode, reqErr.Code())
	assert.Equal(t, wantMessage, reqErr.Message())
}

func TestJSONBody_ShapesExceptionSuffix(t *testing.T) {
	body := JSONBody("ResourceNotFound", "Cluster does not exist")
	var env jsonError
	require.NoError(t, json.Unmarshal(body, &env))
	assert.Equal(t, "ResourceNotFoundException", env.Type)
	assert.Equal(t, "Cluster does not exist", env.Message)
}

// Codes that already carry the "Exception" suffix (e.g.
// awserrors.ErrorEKSResourceNotFound = "ResourceNotFoundException") must not
// be doubled into ResourceNotFoundExceptionException, which SDK clients reject.
func TestJSONBody_DoesNotDoubleExceptionSuffix(t *testing.T) {
	body := JSONBody("ResourceNotFoundException", "Cluster does not exist")
	var env jsonError
	require.NoError(t, json.Unmarshal(body, &env))
	assert.Equal(t, eks.ErrCodeResourceNotFoundException, env.Type)
}

func TestJSONErrorType(t *testing.T) {
	assert.Equal(t, "ThrottlingException", JSONErrorType("Throttling"))
	assert.Equal(t, "ResourceNotFoundException", JSONErrorType("ResourceNotFoundException"))
}

func TestRequestSignalsJSONProtocol(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(r *http.Request)
		signal bool
	}{
		{"X-Amz-Target present", func(r *http.Request) { r.Header.Set("X-Amz-Target", "Whatever.Action") }, true},
		{"json content type", func(r *http.Request) { r.Header.Set("Content-Type", "application/x-amz-json-1.1") }, true},
		{"neither header set", func(r *http.Request) {}, false},
		{"unrelated content type", func(r *http.Request) { r.Header.Set("Content-Type", "application/xml") }, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", nil)
			tc.setup(r)
			assert.Equal(t, tc.signal, RequestSignalsJSONProtocol(r))
		})
	}
}

func TestWriteJSON_WireBytesPerProtocol(t *testing.T) {
	cases := []struct {
		name         string
		p            Protocol
		e            JSONError
		contentType  string
		headers      map[string]string
		absentHeader string
		body         string
	}{
		{
			name:        "JSON 1.1 appends Exception",
			p:           ProtocolJSON11,
			e:           JSONError{Code: "Throttling", Message: "slow", Status: 400},
			contentType: "application/x-amz-json-1.1",
			headers:     map[string]string{"X-Amzn-Errortype": "ThrottlingException"},
			body:        `{"__type":"ThrottlingException","message":"slow"}`,
		},
		{
			name:        "REST-JSON keeps a verbatim type and sorts extra fields",
			p:           ProtocolRESTJSON,
			e:           JSONError{Code: "TooManyRequestsException", Verbatim: true, Message: "Rate Exceeded.", Status: 429, Fields: map[string]string{"Reason": "CallerRateLimitExceeded"}},
			contentType: "application/json",
			headers:     map[string]string{"X-Amzn-Errortype": "TooManyRequestsException"},
			body:        `{"Reason":"CallerRateLimitExceeded","Type":"User","message":"Rate Exceeded."}`,
		},
		{
			name:        "REST-JSON common error gets Exception and a 5xx is a Service fault",
			p:           ProtocolRESTJSON,
			e:           JSONError{Code: "ServiceUnavailable", Message: "down", Status: 503},
			contentType: "application/json",
			headers:     map[string]string{"X-Amzn-Errortype": "ServiceUnavailableException"},
			body:        `{"Type":"Service","message":"down"}`,
		},
		{
			name:        "REST-JSON extra fields cannot replace reserved keys",
			p:           ProtocolRESTJSON,
			e:           JSONError{Code: "X", Verbatim: true, Message: "m", Status: 400, Fields: map[string]string{"Type": "Evil", "message": "evil"}},
			contentType: "application/json",
			body:        `{"Type":"User","message":"m"}`,
		},
		{
			name:         "JSON 1.0 never suffixes and sends the query header",
			p:            ProtocolJSON10Query,
			e:            JSONError{Code: "QueueDoesNotExist", Verbatim: true, Message: "gone", Status: 400, QueryCode: "AWS.SimpleQueueService.NonExistentQueue"},
			contentType:  "application/x-amz-json-1.0",
			headers:      map[string]string{"X-Amzn-Query-Error": "AWS.SimpleQueueService.NonExistentQueue;Sender"},
			absentHeader: "X-Amzn-Errortype",
			body:         `{"__type":"QueueDoesNotExist","message":"gone"}`,
		},
		{
			name:        "JSON 1.0 namespaces a verbatim type only in the body",
			p:           ProtocolJSON10Query,
			e:           JSONError{Code: "QueueDoesNotExist", Verbatim: true, Namespace: "com.amazonaws.sqs", Message: "gone", Status: 400},
			contentType: "application/x-amz-json-1.0",
			headers:     map[string]string{"X-Amzn-Query-Error": "QueueDoesNotExist;Sender"},
			body:        `{"__type":"com.amazonaws.sqs#QueueDoesNotExist","message":"gone"}`,
		},
		{
			name:        "JSON 1.0 leaves a suffixed common error unnamespaced",
			p:           ProtocolJSON10Query,
			e:           JSONError{Code: "ServiceUnavailable", Namespace: "com.amazonaws.sqs", Message: "down", Status: 503},
			contentType: "application/x-amz-json-1.0",
			body:        `{"__type":"ServiceUnavailableException","message":"down"}`,
		},
		{
			name:        "JSON 1.0 query code defaults to the code and a 5xx is a Receiver",
			p:           ProtocolJSON10Query,
			e:           JSONError{Code: "InternalError", Verbatim: true, Message: "boom", Status: 500},
			contentType: "application/x-amz-json-1.0",
			headers:     map[string]string{"X-Amzn-Query-Error": "InternalError;Receiver"},
			body:        `{"__type":"InternalError","message":"boom"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			WriteJSON(w, tc.p, tc.e)
			resp := w.Result()
			assert.Equal(t, tc.e.Status, resp.StatusCode)
			assert.Equal(t, tc.contentType, resp.Header.Get("Content-Type"))
			for k, v := range tc.headers {
				assert.Equal(t, v, resp.Header.Get(k), k)
			}
			if tc.absentHeader != "" {
				assert.Empty(t, resp.Header.Get(tc.absentHeader))
			}
			assert.Equal(t, tc.body, w.Body.String())
		})
	}
}
