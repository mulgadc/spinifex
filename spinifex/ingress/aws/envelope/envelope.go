// Package envelope renders the EC2, IAM-style and S3 XML error envelopes and
// the AWS JSON 1.1 error body. It reads no request context and holds no
// per-service table: callers choose the shape.
package envelope

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// JSONContentType is the AWS JSON 1.1 content type JSON-protocol error bodies
// use (EKS, ECS, ACM, tagging, the bedrock family, and any unserved scope a
// client signals JSON for).
const JSONContentType = "application/x-amz-json-1.1"

// XMLContentType is the content type every XML error envelope uses.
const XMLContentType = "application/xml"

// ec2ErrorResponse is the EC2 query-API error envelope.
// aws-sdk-go v1's ec2query handler rejects the IAM-style <ErrorResponse> envelope
// with SerializationError, so EC2 errors must use <Response><Errors>...</Errors></Response>.
type ec2ErrorResponse struct {
	XMLName   xml.Name  `xml:"Response"`
	Errors    ec2Errors `xml:"Errors"`
	RequestID string    `xml:"RequestID"`
}

type ec2Errors struct {
	Error errorDetail `xml:"Error"`
}

type errorDetail struct {
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// EC2Body builds the EC2 query-API error envelope.
func EC2Body(code, message, requestID string) (output []byte) {
	errorXml := ec2ErrorResponse{
		Errors: ec2Errors{
			Error: errorDetail{
				Code:    code,
				Message: message,
			},
		},
		RequestID: requestID,
	}

	output, err := xml.MarshalIndent(errorXml, "", "  ")
	if err != nil {
		slog.Error("envelope: failed to build EC2 error XML", "error", err)
		return []byte(xml.Header + `<Response><Errors><Error><Code>InternalError</Code><Message>Internal error</Message></Error></Errors><RequestID>` + requestID + `</RequestID></Response>`)
	}

	return append([]byte(xml.Header), output...)
}

// iamErrorResponse is the IAM/STS error XML envelope.
type iamErrorResponse struct {
	XMLName   xml.Name       `xml:"ErrorResponse"`
	Error     iamErrorDetail `xml:"Error"`
	RequestID string         `xml:"RequestId"`
}

type iamErrorDetail struct {
	Type    string `xml:"Type"`
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// IAMBody builds the generic REST-XML/AWS-query ErrorResponse envelope. This
// is the default shape for every XML scope with no dedicated envelope,
// including scopes the gateway does not serve.
func IAMBody(code, message, requestID string) (output []byte) {
	errorXml := iamErrorResponse{
		Error: iamErrorDetail{
			Type:    "Sender",
			Code:    code,
			Message: message,
		},
		RequestID: requestID,
	}

	output, err := xml.MarshalIndent(errorXml, "", "  ")
	if err != nil {
		slog.Error("envelope: failed to build IAM error XML", "error", err)
		return []byte(xml.Header + "<ErrorResponse><Error><Type>Sender</Type><Code>InternalError</Code><Message>Internal error</Message></Error><RequestId>" + requestID + "</RequestId></ErrorResponse>")
	}

	return append([]byte(xml.Header), output...)
}

// s3ErrorResponse is the S3 REST error envelope: a flat <Error> document
// rather than the query-API wrapper. SDKs look for a top-level <Error><Code>
// on an S3 response and report an empty code for anything else.
type s3ErrorResponse struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource,omitempty"`
	RequestID string   `xml:"RequestId"`
}

// S3Body builds the flat S3 REST error document. resource is the request path
// and is omitted when empty.
func S3Body(code, message, requestID, resource string) (output []byte) {
	errorXml := s3ErrorResponse{
		Code:      code,
		Message:   message,
		Resource:  resource,
		RequestID: requestID,
	}

	output, err := xml.MarshalIndent(errorXml, "", "  ")
	if err != nil {
		slog.Error("envelope: failed to build S3 error XML", "error", err)
		return []byte(xml.Header + "<Error><Code>InternalError</Code><Message>Internal error</Message><RequestId>" + requestID + "</RequestId></Error>")
	}

	return append([]byte(xml.Header), output...)
}

// jsonError is the AWS JSON 1.1 error envelope. SDKs key off __type for
// awserr.Code() and message for awserr.Message().
type jsonError struct {
	Type    string `json:"__type"`
	Message string `json:"message"`
}

// JSONBody marshals the AWS JSON 1.1 error body. "Exception" is appended only
// when the code lacks it, avoiding the "ExceptionException" the SDK rejects.
func JSONBody(code, message string) []byte {
	body, err := json.Marshal(jsonError{
		Type:    JSONErrorType(code),
		Message: message,
	})
	if err != nil {
		slog.Error("envelope: failed to marshal JSON error", "code", code, "err", err)
		return fmt.Appendf(nil, `{"__type":"InternalErrorException","message":%q}`, message)
	}
	return body
}

// JSONErrorType derives the X-Amzn-Errortype header value for code, mirroring
// JSONBody's own "Exception" suffixing so the header and the body's __type
// always agree.
func JSONErrorType(code string) string {
	if strings.HasSuffix(code, "Exception") {
		return code
	}
	return code + "Exception"
}

// RequestSignalsJSONProtocol reports whether r's headers mark it as AWS
// JSON-1.x, for a scope no service table knows: JSON-1.1 actions carry
// X-Amz-Target and JSON clients send an application/x-amz-json-* type.
func RequestSignalsJSONProtocol(r *http.Request) bool {
	if r.Header.Get("X-Amz-Target") != "" {
		return true
	}
	return strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-amz-json")
}
