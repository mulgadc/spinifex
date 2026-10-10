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
