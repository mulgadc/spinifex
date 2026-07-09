package gateway

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	awscreds "github.com/aws/aws-sdk-go/aws/credentials"
	awssession "github.com/aws/aws-sdk-go/aws/session"
	awsec2 "github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/predastore/ratelimit"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// doRequest sends a request through an http.Handler and returns the response.
func doRequest(handler http.Handler, req *http.Request) *http.Response {
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w.Result()
}

func TestGenerateEC2ErrorResponse_Structure(t *testing.T) {
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
			output := GenerateEC2ErrorResponse(tc.code, tc.message, tc.requestID)
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

func TestGenerateEC2ErrorResponse_ValidXML(t *testing.T) {
	output := GenerateEC2ErrorResponse("TestCode", "Test message", "req-999")
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

func TestGenerateIAMErrorResponse_Structure(t *testing.T) {
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
			output := GenerateIAMErrorResponse(tc.code, tc.message, tc.requestID)
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

func TestGenerateIAMErrorResponse_ValidXML(t *testing.T) {
	output := GenerateIAMErrorResponse("NoSuchEntity", "Entity not found", "req-iam-999")
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

func TestErrorHandler_IAMService(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), ctxService, "iam")
		r = r.WithContext(ctx)
		gw.ErrorHandler(w, r, errors.New(awserrors.ErrorIAMNoSuchEntity))
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	resp := doRequest(handler, req)
	assert.Equal(t, 404, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	xmlStr := string(body)
	// IAM format uses <ErrorResponse> not <Response>
	assert.Contains(t, xmlStr, "<ErrorResponse>")
	assert.Contains(t, xmlStr, "<Type>Sender</Type>")
	assert.Contains(t, xmlStr, "<Code>NoSuchEntity</Code>")
	assert.NotContains(t, xmlStr, "<Errors>")
}

func TestErrorHandler_UnknownError(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), ctxService, "ec2")
		r = r.WithContext(ctx)
		gw.ErrorHandler(w, r, errors.New("SomeCompletelyBogusError"))
	})

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	resp := doRequest(handler, req)
	assert.Equal(t, 500, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	xmlStr := string(body)
	// Unknown errors should be remapped to InternalError
	assert.Contains(t, xmlStr, "<Code>InternalError</Code>")
	assert.Contains(t, xmlStr, "<Response>")
	assert.Contains(t, xmlStr, "<Errors>")
}

func TestErrorHandler_ELBv2Service(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), ctxService, "elasticloadbalancing")
		r = r.WithContext(ctx)
		gw.ErrorHandler(w, r, errors.New(awserrors.ErrorInvalidAction))
	})

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	resp := doRequest(handler, req)
	assert.Equal(t, 400, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	xmlStr := string(body)
	// ELBv2 uses the IAM-style Query envelope; EC2 shape breaks SDK Code parsing.
	assert.Contains(t, xmlStr, "<ErrorResponse>")
	assert.Contains(t, xmlStr, "<Type>Sender</Type>")
	assert.Contains(t, xmlStr, "<Code>InvalidAction</Code>")
	assert.NotContains(t, xmlStr, "<Errors>")
}

func TestErrorHandler_EC2Service(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), ctxService, "ec2")
		r = r.WithContext(ctx)
		gw.ErrorHandler(w, r, errors.New(awserrors.ErrorInvalidParameterValue))
	})

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	resp := doRequest(handler, req)
	assert.Equal(t, 400, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	xmlStr := string(body)
	assert.Contains(t, xmlStr, "<Response>")
	assert.Contains(t, xmlStr, "<Errors>")
	assert.Contains(t, xmlStr, "<Code>InvalidParameterValue</Code>")
}

func TestErrorHandler_IgnoresClientRequestID(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), ctxService, "ec2")
		r = r.WithContext(ctx)
		gw.ErrorHandler(w, r, errors.New(awserrors.ErrorInternalError))
	})

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Amz-Request-Id", "custom-req-id-123")
	resp := doRequest(handler, req)

	body, _ := io.ReadAll(resp.Body)
	assert.NotContains(t, string(body), "custom-req-id-123")
	assert.Contains(t, string(body), "<RequestID>")
}

func TestErrorHandler_ContentTypeXML(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), ctxService, "ec2")
		r = r.WithContext(ctx)
		gw.ErrorHandler(w, r, errors.New(awserrors.ErrorInternalError))
	})

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	resp := doRequest(handler, req)
	assert.Equal(t, "application/xml", resp.Header.Get("Content-Type"))
}

func startTestNATS(t *testing.T) *nats.Conn {
	t.Helper()
	_, nc := testutil.StartTestNATS(t)
	return nc
}

func TestDiscoverActiveNodes_NilNATS(t *testing.T) {
	gw := &GatewayConfig{
		ExpectedNodes: 3,
		NATSConn:      nil,
	}

	result := gw.DiscoverActiveNodes(context.Background())
	assert.Equal(t, 3, result)
}

func TestDiscoverActiveNodes_NoResponders(t *testing.T) {
	nc := startTestNATS(t)

	gw := &GatewayConfig{
		ExpectedNodes: 5,
		NATSConn:      nc,
	}

	result := gw.DiscoverActiveNodes(context.Background())
	assert.Equal(t, 5, result)
}

func TestDiscoverActiveNodes_WithResponders(t *testing.T) {
	nc := startTestNATS(t)

	for _, nodeName := range []string{"node-1", "node-2"} {
		name := nodeName
		_, err := nc.Subscribe("spinifex.nodes.discover", func(msg *nats.Msg) {
			resp := types.NodeDiscoverResponse{Node: name}
			data, _ := json.Marshal(resp)
			msg.Respond(data)
		})
		require.NoError(t, err)
	}
	require.NoError(t, nc.Flush())

	gw := &GatewayConfig{
		ExpectedNodes: 1,
		NATSConn:      nc,
	}

	result := gw.DiscoverActiveNodes(context.Background())
	assert.Equal(t, 2, result)
}

func TestDiscoverActiveNodes_InvalidJSON(t *testing.T) {
	nc := startTestNATS(t)

	_, err := nc.Subscribe("spinifex.nodes.discover", func(msg *nats.Msg) {
		msg.Respond([]byte("not json"))
	})
	require.NoError(t, err)
	require.NoError(t, nc.Flush())

	gw := &GatewayConfig{
		ExpectedNodes: 4,
		NATSConn:      nc,
	}

	result := gw.DiscoverActiveNodes(context.Background())
	assert.Equal(t, 4, result)
}

func TestDiscoverActiveNodes_DuplicateNodes(t *testing.T) {
	nc := startTestNATS(t)

	for range 2 {
		_, err := nc.Subscribe("spinifex.nodes.discover", func(msg *nats.Msg) {
			resp := types.NodeDiscoverResponse{Node: "same-node"}
			data, _ := json.Marshal(resp)
			msg.Respond(data)
		})
		require.NoError(t, err)
	}
	require.NoError(t, nc.Flush())

	gw := &GatewayConfig{
		ExpectedNodes: 5,
		NATSConn:      nc,
	}

	result := gw.DiscoverActiveNodes(context.Background())
	assert.Equal(t, 1, result)
}

func TestParseAWSQueryArgs(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		expected map[string]string
	}{
		{
			name:  "simple action and version",
			query: "Action=DescribeInstances&Version=2016-11-15",
			expected: map[string]string{
				"Action":  "DescribeInstances",
				"Version": "2016-11-15",
			},
		},
		{
			name:  "URL-encoded values",
			query: "Name=%2Fdev%2Fsda&Value=hello%20world",
			expected: map[string]string{
				"Name":  "/dev/sda",
				"Value": "hello world",
			},
		},
		{
			name:  "key without value",
			query: "DryRun",
			expected: map[string]string{
				"DryRun": "",
			},
		},
		{
			name:     "empty string",
			query:    "",
			expected: map[string]string{"": ""},
		},
		{
			name:  "multiple parameters",
			query: "Action=RunInstances&ImageId=ami-123&MinCount=1&MaxCount=5&InstanceType=t2.micro",
			expected: map[string]string{
				"Action":       "RunInstances",
				"ImageId":      "ami-123",
				"MinCount":     "1",
				"MaxCount":     "5",
				"InstanceType": "t2.micro",
			},
		},
		{
			name:  "value containing equals sign",
			query: "Filter.1.Name=tag:Env&Filter.1.Value=prod=staging",
			expected: map[string]string{
				"Filter.1.Name":  "tag:Env",
				"Filter.1.Value": "prod=staging",
			},
		},
		{
			name:  "URL-encoded key and value",
			query: "Tag%2EName=my%20tag",
			expected: map[string]string{
				"Tag.Name": "my tag",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := ParseAWSQueryArgs(tc.query)
			assert.NoError(t, err)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestParseAWSQueryArgs_MalformedURLEncoding(t *testing.T) {
	// AWS returns MalformedQueryString for invalid percent-encoding; the parser
	// must surface an error instead of silently dropping the bad pair.
	tests := []struct {
		name  string
		query string
	}{
		{"bad value encoding", "Action=DescribeInstances&Name=%ZZ"},
		{"bad key encoding", "Bad%ZZKey=value"},
		{"bad lone key encoding", "Lone%ZZ"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseAWSQueryArgs(tc.query)
			require.Error(t, err)
		})
	}
}

func TestGetService(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}

	tests := []struct {
		name      string
		ctxVal    any // value to set in ctxService, nil means no value
		wantSvc   string
		wantError string
	}{
		{
			name:      "no service in context",
			ctxVal:    nil,
			wantError: awserrors.ErrorAuthFailure,
		},
		{
			name:      "non-string value in context",
			ctxVal:    12345,
			wantError: awserrors.ErrorAuthFailure,
		},
		{
			name:      "unsupported service",
			ctxVal:    "s3",
			wantError: awserrors.ErrorUnsupportedOperation,
		},
		{
			name:    "ec2 service",
			ctxVal:  "ec2",
			wantSvc: "ec2",
		},
		{
			name:    "iam service",
			ctxVal:  "iam",
			wantSvc: "iam",
		},
		{
			name:    "tagging service",
			ctxVal:  "tagging",
			wantSvc: "tagging",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			if tc.ctxVal != nil {
				ctx := context.WithValue(req.Context(), ctxService, tc.ctxVal)
				req = req.WithContext(ctx)
			}

			svc, err := gw.GetService(req)
			if tc.wantError != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantError, err.Error())
				assert.Empty(t, svc)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.wantSvc, svc)
			}
		})
	}
}

func TestRequest_NoServiceContext(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	w := httptest.NewRecorder()

	gw.Request(w, req)

	resp := w.Result()
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, 403, resp.StatusCode)
	assert.Contains(t, string(body), "AuthFailure")
}

func TestRequest_UnsupportedService(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	ctx := context.WithValue(req.Context(), ctxService, "s3")
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	gw.Request(w, req)

	resp := w.Result()
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, 400, resp.StatusCode)
	assert.Contains(t, string(body), "UnsupportedOperation")
}

func TestRequest_MalformedQueryString_EndToEnd(t *testing.T) {
	tests := []struct {
		name    string
		service string
		body    string
	}{
		{"ec2", "ec2", "Action=DescribeInstances&Bad=%ZZ"},
		{"elbv2", "elasticloadbalancing", "Action=DescribeLoadBalancers&Bad=%ZZ"},
		{"iam", "iam", "Action=ListUsers&Bad=%ZZ"},
		{"spinifex", "spinifex", "Action=GetVersion&Bad=%ZZ"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// A live NATS connection bypasses the cluster-unavailable gate.
			gw := &GatewayConfig{DisableLogging: true, NATSConn: connectedNATS(t)}
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			ctx := context.WithValue(req.Context(), ctxService, tc.service)
			ctx = context.WithValue(ctx, ctxAccountID, "123456789012")
			req = req.WithContext(ctx)
			w := httptest.NewRecorder()

			gw.Request(w, req)

			resp := w.Result()
			body, _ := io.ReadAll(resp.Body)
			assert.Equal(t, 400, resp.StatusCode)
			assert.Contains(t, string(body), "MalformedQueryString")
		})
	}
}

func TestRequest_EC2MissingAction(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, NATSConn: connectedNATS(t)}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(""))
	ctx := context.WithValue(req.Context(), ctxService, "ec2")
	ctx = context.WithValue(ctx, ctxAccountID, "123456789012")
	ctx = context.WithValue(ctx, ctxPrincipalType, principalTypeUser)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	gw.Request(w, req)

	resp := w.Result()
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, 400, resp.StatusCode)
	assert.Contains(t, string(body), "MissingAction")
}

func TestRequest_IAMNilService(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, IAMService: nil, NATSConn: connectedNATS(t)}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("Action=CreateUser&UserName=test"))
	ctx := context.WithValue(req.Context(), ctxService, "iam")
	ctx = context.WithValue(ctx, ctxAccountID, "123456789012")
	ctx = context.WithValue(ctx, ctxPrincipalType, principalTypeUser)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	gw.Request(w, req)

	resp := w.Result()
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, 500, resp.StatusCode)
	assert.Contains(t, string(body), "InternalError")
}

// connectedNATS returns a live test NATS connection for short-circuit-bypass
// tests that exercise per-service handlers without actually publishing.
func connectedNATS(t *testing.T) *nats.Conn {
	t.Helper()
	ns, _ := testutil.StartTestNATS(t)
	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	return nc
}

// setupEC2Request creates an http.Request with EC2 service context and optional account ID.
func setupEC2Request(body string, accountID string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	ctx := context.WithValue(req.Context(), ctxService, "ec2")
	if accountID != "" {
		ctx = context.WithValue(ctx, ctxAccountID, accountID)
	}
	req = req.WithContext(ctx)
	return req
}

func TestEC2Request_MissingAction(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	req := setupEC2Request("", "123456789012")
	w := httptest.NewRecorder()

	err := gw.EC2_Request(w, req)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingAction, err.Error())
}

func TestEC2Request_UnknownAction(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	req := setupEC2Request("Action=FakeAction", "123456789012")
	w := httptest.NewRecorder()

	err := gw.EC2_Request(w, req)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAction, err.Error())
}

func TestEC2Request_NilNATSNonLocalAction(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, NATSConn: nil}
	req := setupEC2Request("Action=DescribeInstances", "123456789012")
	w := httptest.NewRecorder()

	err := gw.EC2_Request(w, req)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestEC2Request_NilNATSLocalAction(t *testing.T) {
	gw := &GatewayConfig{
		DisableLogging: true,
		NATSConn:       nil,
		Region:         "us-east-1",
		AZ:             "us-east-1a",
	}
	req := setupEC2Request("Action=DescribeRegions", "123456789012")
	w := httptest.NewRecorder()

	err := gw.EC2_Request(w, req)
	require.NoError(t, err)

	resp := w.Result()
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, "text/xml", resp.Header.Get("Content-Type"))

	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "DescribeRegionsResponse")
}

func TestEC2Request_MissingAccountID(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, NATSConn: nil}
	// Use a local action so we don't fail on nil NATS first
	req := setupEC2Request("Action=DescribeRegions", "")
	w := httptest.NewRecorder()

	err := gw.EC2_Request(w, req)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestEC2Request_DescribeAccountAttributes(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, NATSConn: nil}
	req := setupEC2Request("Action=DescribeAccountAttributes", "123456789012")
	w := httptest.NewRecorder()

	err := gw.EC2_Request(w, req)
	require.NoError(t, err)

	resp := w.Result()
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, "text/xml", resp.Header.Get("Content-Type"))

	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "DescribeAccountAttributesResponse")
}

func TestEC2Request_DescribeAvailabilityZones(t *testing.T) {
	gw := &GatewayConfig{
		DisableLogging: true,
		NATSConn:       nil,
		Region:         "us-east-1",
		AZ:             "us-east-1a",
	}
	req := setupEC2Request("Action=DescribeAvailabilityZones", "123456789012")
	w := httptest.NewRecorder()

	err := gw.EC2_Request(w, req)
	require.NoError(t, err)

	resp := w.Result()
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, "text/xml", resp.Header.Get("Content-Type"))

	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "DescribeAvailabilityZonesResponse")
}

func TestCheckPolicy_NilIAMService(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, IAMService: nil}
	req := httptest.NewRequest(http.MethodPost, "/", nil)

	err := gw.checkPolicy(req, "ec2", "DescribeInstances")
	assert.NoError(t, err)
}

func TestCheckPolicy_NoIdentityInContext(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, IAMService: &mockIAMService{}}
	req := httptest.NewRequest(http.MethodPost, "/", nil)

	err := gw.checkPolicy(req, "ec2", "DescribeInstances")
	assert.NoError(t, err)
}

func TestCheckPolicy_RootUserGlobalAccount(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, IAMService: &mockIAMService{}}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	ctx := context.WithValue(req.Context(), ctxIdentity, "root")
	ctx = context.WithValue(ctx, ctxAccountID, "000000000000") // GlobalAccountID
	ctx = context.WithValue(ctx, ctxPrincipalType, principalTypeUser)
	req = req.WithContext(ctx)

	err := gw.checkPolicy(req, "ec2", "DescribeInstances")
	assert.NoError(t, err)
}

// TestCheckPolicy_AssumedRoleSessionNamedRoot ensures the principal-type gate
// fires BEFORE the identity-string root short-circuit. A session whose
// SessionName is "root" must not inherit root privileges.
func TestCheckPolicy_AssumedRoleSessionNamedRoot(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, IAMService: &mockIAMService{}}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	ctx := context.WithValue(req.Context(), ctxIdentity, "root")
	ctx = context.WithValue(ctx, ctxAccountID, "000000000000")
	ctx = context.WithValue(ctx, ctxPrincipalType, principalTypeAssumedRole)
	ctx = context.WithValue(ctx, ctxAssumedRoleARN, "arn:aws:sts::000000000000:assumed-role/r/root")
	req = req.WithContext(ctx)

	err := gw.checkPolicy(req, "ec2", "DescribeInstances")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
}

func TestCheckPolicy_NonRootAllowPolicy(t *testing.T) {
	mock := &policyMockIAMService{
		getUserPoliciesFn: func(_, _ string) ([]handlers_iam.PolicyDocument, error) {
			return []handlers_iam.PolicyDocument{
				{
					Version: "2012-10-17",
					Statement: []handlers_iam.Statement{
						{Effect: "Allow", Action: handlers_iam.StringOrArr{"ec2:*"}, Resource: handlers_iam.StringOrArr{"*"}},
					},
				},
			}, nil
		},
	}
	gw := &GatewayConfig{DisableLogging: true, IAMService: mock}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	ctx := context.WithValue(req.Context(), ctxIdentity, "alice")
	ctx = context.WithValue(ctx, ctxAccountID, "123456789012")
	ctx = context.WithValue(ctx, ctxPrincipalType, principalTypeUser)
	req = req.WithContext(ctx)

	err := gw.checkPolicy(req, "ec2", "DescribeInstances")
	assert.NoError(t, err)
}

func TestCheckPolicy_NonRootDenyPolicy(t *testing.T) {
	mock := &policyMockIAMService{
		getUserPoliciesFn: func(_, _ string) ([]handlers_iam.PolicyDocument, error) {
			return []handlers_iam.PolicyDocument{
				{
					Version: "2012-10-17",
					Statement: []handlers_iam.Statement{
						{Effect: "Deny", Action: handlers_iam.StringOrArr{"ec2:*"}, Resource: handlers_iam.StringOrArr{"*"}},
					},
				},
			}, nil
		},
	}
	gw := &GatewayConfig{DisableLogging: true, IAMService: mock}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	ctx := context.WithValue(req.Context(), ctxIdentity, "alice")
	ctx = context.WithValue(ctx, ctxAccountID, "123456789012")
	ctx = context.WithValue(ctx, ctxPrincipalType, principalTypeUser)
	req = req.WithContext(ctx)

	err := gw.checkPolicy(req, "ec2", "DescribeInstances")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
}

func TestCheckPolicy_NonRootNoPolicies(t *testing.T) {
	mock := &policyMockIAMService{
		getUserPoliciesFn: func(_, _ string) ([]handlers_iam.PolicyDocument, error) {
			return nil, nil
		},
	}
	gw := &GatewayConfig{DisableLogging: true, IAMService: mock}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	ctx := context.WithValue(req.Context(), ctxIdentity, "alice")
	ctx = context.WithValue(ctx, ctxAccountID, "123456789012")
	ctx = context.WithValue(ctx, ctxPrincipalType, principalTypeUser)
	req = req.WithContext(ctx)

	err := gw.checkPolicy(req, "ec2", "DescribeInstances")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
}

func TestCheckPolicy_GetUserPoliciesError(t *testing.T) {
	mock := &policyMockIAMService{
		getUserPoliciesFn: func(_, _ string) ([]handlers_iam.PolicyDocument, error) {
			return nil, errors.New("db connection failed")
		},
	}
	gw := &GatewayConfig{DisableLogging: true, IAMService: mock}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	ctx := context.WithValue(req.Context(), ctxIdentity, "alice")
	ctx = context.WithValue(ctx, ctxAccountID, "123456789012")
	ctx = context.WithValue(ctx, ctxPrincipalType, principalTypeUser)
	req = req.WithContext(ctx)

	err := gw.checkPolicy(req, "ec2", "DescribeInstances")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInternalError, err.Error())
}

func TestCheckPolicy_EmptyIdentity(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, IAMService: &mockIAMService{}}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	ctx := context.WithValue(req.Context(), ctxIdentity, "")
	ctx = context.WithValue(ctx, ctxAccountID, "123456789012")
	ctx = context.WithValue(ctx, ctxPrincipalType, principalTypeUser)
	req = req.WithContext(ctx)

	err := gw.checkPolicy(req, "ec2", "DescribeInstances")
	assert.NoError(t, err)
}

func TestCheckPolicy_MissingAccountID(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, IAMService: &mockIAMService{}}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	ctx := context.WithValue(req.Context(), ctxIdentity, "alice")
	// No account ID
	req = req.WithContext(ctx)

	err := gw.checkPolicy(req, "ec2", "DescribeInstances")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInternalError, err.Error())
}

func TestCheckPolicy_NATSTransientRetriesAllAttempts(t *testing.T) {
	calls := 0
	mock := &policyMockIAMService{
		getUserPoliciesFn: func(_, _ string) ([]handlers_iam.PolicyDocument, error) {
			calls++
			return nil, fmt.Errorf("get user: %w", nats.ErrNoResponders)
		},
	}
	gw := &GatewayConfig{DisableLogging: true, IAMService: mock}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	ctx := context.WithValue(req.Context(), ctxIdentity, "alice")
	ctx = context.WithValue(ctx, ctxAccountID, "123456789012")
	ctx = context.WithValue(ctx, ctxPrincipalType, principalTypeUser)
	req = req.WithContext(ctx)

	err := gw.checkPolicy(req, "ec2", "DescribeInstances")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInternalError, err.Error())
	assert.Equal(t, 3, calls, "should have retried all 3 attempts")
}

func TestCheckPolicy_NATSTransientRetriesThenSucceeds(t *testing.T) {
	calls := 0
	mock := &policyMockIAMService{
		getUserPoliciesFn: func(_, _ string) ([]handlers_iam.PolicyDocument, error) {
			calls++
			if calls < 3 {
				return nil, fmt.Errorf("get user: %w", nats.ErrNoResponders)
			}
			return []handlers_iam.PolicyDocument{
				{
					Version: "2012-10-17",
					Statement: []handlers_iam.Statement{
						{Effect: "Allow", Action: handlers_iam.StringOrArr{"ec2:*"}, Resource: handlers_iam.StringOrArr{"*"}},
					},
				},
			}, nil
		},
	}
	gw := &GatewayConfig{DisableLogging: true, IAMService: mock}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	ctx := context.WithValue(req.Context(), ctxIdentity, "alice")
	ctx = context.WithValue(ctx, ctxAccountID, "123456789012")
	ctx = context.WithValue(ctx, ctxPrincipalType, principalTypeUser)
	req = req.WithContext(ctx)

	err := gw.checkPolicy(req, "ec2", "DescribeInstances")
	assert.NoError(t, err)
	assert.Equal(t, 3, calls, "should have retried until success")
}

func TestCheckPolicy_NonTransientErrorStillFails(t *testing.T) {
	mock := &policyMockIAMService{
		getUserPoliciesFn: func(_, _ string) ([]handlers_iam.PolicyDocument, error) {
			return nil, errors.New("database corruption")
		},
	}
	gw := &GatewayConfig{DisableLogging: true, IAMService: mock}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	ctx := context.WithValue(req.Context(), ctxIdentity, "alice")
	ctx = context.WithValue(ctx, ctxAccountID, "123456789012")
	ctx = context.WithValue(ctx, ctxPrincipalType, principalTypeUser)
	req = req.WithContext(ctx)

	err := gw.checkPolicy(req, "ec2", "DescribeInstances")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInternalError, err.Error())
}

func TestIsNATSTransient(t *testing.T) {
	assert.False(t, isNATSTransient(nil))
	assert.True(t, isNATSTransient(nats.ErrNoResponders))
	assert.True(t, isNATSTransient(nats.ErrTimeout))
	assert.True(t, isNATSTransient(fmt.Errorf("get user: %w", nats.ErrNoResponders)))
	assert.False(t, isNATSTransient(errors.New("some other error")))
}

func TestImportKeyPair_Base64PaddingWorkaround(t *testing.T) {
	// The ImportKeyPair handler decodes URL-encoded Base64 padding (%3D%3D → ==)
	// before passing to the generic handler.
	handler := ec2Actions["ImportKeyPair"]
	require.NotNil(t, handler)

	q := map[string]string{
		"Action":            "ImportKeyPair",
		"KeyName":           "test-key",
		"PublicKeyMaterial": "c3NoLXJzYSBBQUFBQjNOemFDMXljMkVBQUFBREFRQUJBQUFCZ1FD%3D%3D",
	}

	gw := &GatewayConfig{DisableLogging: true, NATSConn: nil}
	// NATS is nil so the handler errors, but PublicKeyMaterial is modified before that.
	_, _ = handler("ImportKeyPair", q, gw, "123456789012", nil)

	assert.True(t, strings.HasSuffix(q["PublicKeyMaterial"], "=="),
		"Expected PublicKeyMaterial to end with == but got: %s", q["PublicKeyMaterial"])
	assert.False(t, strings.Contains(q["PublicKeyMaterial"], "%3D"),
		"Expected no URL-encoded padding remaining")
}

// --- Throttle middleware integration tests ---

func TestThrottleKeyFuncs_ExtractsAccountAndAction(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	keyFuncs := gw.throttleKeyFuncs()
	require.Len(t, keyFuncs, 2)

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	ctx := context.WithValue(req.Context(), ctxAccountID, "123456789012")
	ctx = context.WithValue(ctx, ctxAction, "DescribeInstances")
	req = req.WithContext(ctx)

	acct, err := keyFuncs[0](req)
	require.NoError(t, err)
	assert.Equal(t, "123456789012", acct)

	action, err := keyFuncs[1](req)
	require.NoError(t, err)
	assert.Equal(t, "DescribeInstances", action)
}

func TestThrottleKeyFuncs_UnknownAction(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	keyFuncs := gw.throttleKeyFuncs()

	// No ctxAction in context — should return "unknown".
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	ctx := context.WithValue(req.Context(), ctxAccountID, "123")
	req = req.WithContext(ctx)

	action, err := keyFuncs[1](req)
	require.NoError(t, err)
	assert.Equal(t, "unknown", action)
}

func TestThrottleKeyFuncs_MissingAccountID(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}
	keyFuncs := gw.throttleKeyFuncs()

	// No ctxAccountID in context — should return an error.
	req := httptest.NewRequest(http.MethodPost, "/", nil)

	_, err := keyFuncs[0](req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "account-id missing")
}

func TestWriteThrottleError_EC2(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	ctx := context.WithValue(req.Context(), ctxService, "ec2")
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	gw.writeThrottleError(w, req)

	resp := w.Result()
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, 503, resp.StatusCode)
	assert.Equal(t, "application/xml", resp.Header.Get("Content-Type"))
	assert.Contains(t, string(body), "<Code>RequestLimitExceeded</Code>")
	assert.Contains(t, string(body), "<Response>")
}

func TestWriteThrottleError_IAM(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	ctx := context.WithValue(req.Context(), ctxService, "iam")
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	gw.writeThrottleError(w, req)

	resp := w.Result()
	body, _ := io.ReadAll(resp.Body)
	// AWS returns 503 for Throttling on every service; TF respects 503 + Retry-After but gives up on 400.
	assert.Equal(t, 503, resp.StatusCode)
	assert.Contains(t, string(body), "<Code>Throttling</Code>")
	assert.Contains(t, string(body), "<ErrorResponse>")
}

func TestThrottleMiddleware_Integration(t *testing.T) {
	cfg := ratelimit.Config{
		Enabled: true,
		Rate:    1,
		Burst:   2,
	}
	throttler := ratelimit.New(cfg)
	defer throttler.Stop()

	gw := &GatewayConfig{
		DisableLogging: true,
		Throttler:      throttler,
	}

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mw := throttler.Middleware(gw.throttleKeyFuncs(), gw.writeThrottleError)
	handler := mw(inner)

	makeReq := func() *http.Response {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		ctx := context.WithValue(req.Context(), ctxAccountID, "acct1")
		ctx = context.WithValue(ctx, ctxService, "ec2")
		ctx = context.WithValue(ctx, ctxAction, "DescribeInstances")
		req = req.WithContext(ctx)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w.Result()
	}

	// First two requests should succeed (burst=2).
	resp1 := makeReq()
	assert.Equal(t, 200, resp1.StatusCode)
	resp2 := makeReq()
	assert.Equal(t, 200, resp2.StatusCode)

	// Third request should be throttled.
	resp3 := makeReq()
	assert.Equal(t, 503, resp3.StatusCode)
	assert.NotEmpty(t, resp3.Header.Get("Retry-After"))

	body3, _ := io.ReadAll(resp3.Body)
	assert.Contains(t, string(body3), "RequestLimitExceeded")
}

func TestThrottleMiddleware_DisabledConfig(t *testing.T) {
	// When Throttler is nil, SetupRoutes skips middleware — no panic.
	gw := &GatewayConfig{DisableLogging: true, Throttler: nil}
	handler := gw.SetupRoutes()

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("Action=DescribeInstances"))
	ctx := context.WithValue(req.Context(), ctxAccountID, "acct1")
	ctx = context.WithValue(ctx, ctxService, "ec2")
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Without auth the request fails on SigV4, not throttling.
	resp := w.Result()
	assert.NotEqual(t, 503, resp.StatusCode)
}

func TestThrottleMiddleware_PerActionIsolation(t *testing.T) {
	cfg := ratelimit.Config{
		Enabled: true,
		Rate:    1,
		Burst:   1,
	}
	throttler := ratelimit.New(cfg)
	defer throttler.Stop()

	gw := &GatewayConfig{DisableLogging: true}

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mw := throttler.Middleware(gw.throttleKeyFuncs(), gw.writeThrottleError)
	handler := mw(inner)

	makeReq := func(action string) *http.Response {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		ctx := context.WithValue(req.Context(), ctxAccountID, "acct1")
		ctx = context.WithValue(ctx, ctxService, "ec2")
		ctx = context.WithValue(ctx, ctxAction, action)
		req = req.WithContext(ctx)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w.Result()
	}

	// Exhaust DescribeInstances.
	resp := makeReq("DescribeInstances")
	assert.Equal(t, 200, resp.StatusCode)
	resp = makeReq("DescribeInstances")
	assert.Equal(t, 503, resp.StatusCode)

	// RunInstances should be independent.
	resp = makeReq("RunInstances")
	assert.Equal(t, 200, resp.StatusCode)
}

func TestRequest_ClusterUnavailableNilConn_EC2(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	ctx := context.WithValue(req.Context(), ctxService, "ec2")
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	gw.Request(w, req)
	resp := w.Result()

	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	xmlStr := string(body)
	assert.Contains(t, xmlStr, "<Code>ServiceUnavailable</Code>")
	assert.Contains(t, xmlStr, "cluster unavailable: NATS disconnected")
	assert.Contains(t, xmlStr, "/local/status")
	assert.Contains(t, xmlStr, "<Response>") // EC2 XML envelope
}

func TestRequest_ClusterUnavailableNilConn_IAM(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	ctx := context.WithValue(req.Context(), ctxService, "iam")
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	gw.Request(w, req)
	resp := w.Result()

	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	xmlStr := string(body)
	assert.Contains(t, xmlStr, "<Code>ServiceUnavailable</Code>")
	assert.Contains(t, xmlStr, "<ErrorResponse>") // IAM XML envelope
}

func TestRequest_ClusterUnavailableClosedConn(t *testing.T) {
	ns, _ := testutil.StartTestNATS(t)
	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	nc.Close()

	gw := &GatewayConfig{DisableLogging: true, NATSConn: nc}

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	ctx := context.WithValue(req.Context(), ctxService, "ec2")
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	gw.Request(w, req)
	resp := w.Result()

	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

// TestGenerateEC2ErrorResponse_SDKRoundTrip serves the EC2 error envelope from
// an httptest server, points an aws-sdk-go v1 EC2 client at it, and asserts
// the SDK surfaces the code via awserr.Error.Code() — not SerializationError.
// aws-sdk-go v1's ec2query handler rejects the IAM <ErrorResponse> envelope and
// discards the embedded code, so the EC2 <Response>/<Errors> shape is required.
func TestGenerateEC2ErrorResponse_SDKRoundTrip(t *testing.T) {
	const wantCode = "InvalidInstanceType"
	const wantMessage = "The instance type 't2.micro' is not supported in this region."

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body := GenerateEC2ErrorResponse(wantCode, wantMessage, "req-sdk-roundtrip")
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	sess, err := awssession.NewSession(&aws.Config{
		Region:      aws.String("us-east-1"),
		Endpoint:    aws.String(srv.URL),
		Credentials: awscreds.NewStaticCredentials("AKIA-TEST", "secret", ""),
		DisableSSL:  aws.Bool(true),
		// Suppress the default retry loop — error responses are not retryable
		// here and waiting them out wastes test time.
		MaxRetries: aws.Int(0),
	})
	require.NoError(t, err)

	client := awsec2.New(sess)
	_, err = client.RunInstances(&awsec2.RunInstancesInput{
		ImageId:      aws.String("ami-test"),
		InstanceType: aws.String("t2.micro"),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
	})
	require.Error(t, err)

	var awsErr awserr.Error
	require.True(t, errors.As(err, &awsErr), "expected awserr.Error, got %T: %v", err, err)
	assert.Equal(t, wantCode, awsErr.Code())
	assert.NotEqual(t, "SerializationError", awsErr.Code(), "SDK could not parse the envelope")
}
