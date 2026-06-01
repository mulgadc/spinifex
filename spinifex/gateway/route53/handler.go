// Package gateway_route53 holds the HTTP-side glue between awsgw and the
// Route53 handlers package. Route53 speaks AWS REST/XML over /2013-04-01/*
// (route53-v0.md R1). Per-action handlers in zone.go / recordset.go /
// change.go marshal their typed *route53.<X>Output via MarshalResponseXML
// and return the bytes to the top-level dispatcher in gateway/route53.go.
package gateway_route53

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// XMLContentType is the response content type Route53 emits for both
// success and error paths.
const XMLContentType = "application/xml"

// route53XMLNamespace is the XML namespace AWS Route53 advertises in
// every response envelope.
const route53XMLNamespace = "https://route53.amazonaws.com/doc/2013-04-01/"

// ErrorResponse is the AWS Route53 error envelope.
type ErrorResponse struct {
	XMLName   xml.Name    `xml:"ErrorResponse"`
	XMLNS     string      `xml:"xmlns,attr"`
	Error     ErrorDetail `xml:"Error"`
	RequestID string      `xml:"RequestId"`
}

// ErrorDetail mirrors the AWS Route53 <Error> sub-element shape.
type ErrorDetail struct {
	Type    string `xml:"Type"`
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// GenerateErrorResponse marshals the AWS Route53 error envelope.
func GenerateErrorResponse(code, message, requestID string) []byte {
	env := ErrorResponse{
		XMLNS:     route53XMLNamespace,
		Error:     ErrorDetail{Type: "Sender", Code: code, Message: message},
		RequestID: requestID,
	}
	out, err := xml.MarshalIndent(env, "", "  ")
	if err != nil {
		slog.Error("Route53: failed to marshal error envelope", "code", code, "err", err)
		return fmt.Appendf(nil,
			`<?xml version="1.0"?><ErrorResponse xmlns=%q><Error><Type>Sender</Type><Code>%s</Code><Message>%s</Message></Error><RequestId>%s</RequestId></ErrorResponse>`,
			route53XMLNamespace, code, message, requestID)
	}
	return append([]byte(xml.Header), out...)
}

// MarshalResponseXML serialises obj as XML with the Route53 namespace
// prepended via xml.Header. Caller is responsible for matching the
// AWS-shaped XML element names on obj's struct tags.
func MarshalResponseXML(obj any) ([]byte, error) {
	body, err := xml.MarshalIndent(obj, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal route53 xml: %w", err)
	}
	return append([]byte(xml.Header), body...), nil
}

// ReadBody slurps the request body, returning an empty byte slice for
// nil-bodied requests (GET / DELETE typically). Mirrors EKS ParseJSONBody
// shape minus the type-parameter, since each per-action handler picks
// its own decode strategy (URL params vs body XML).
func ReadBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return body, nil
}

// DecodeBodyXML unmarshals body into out, returning
// ErrorMalformedInput-wrapped error on parse failure. Empty body is
// permitted — the typed input stays zero-valued.
func DecodeBodyXML[T any](body []byte) (*T, error) {
	out := new(T)
	if len(body) == 0 {
		return out, nil
	}
	if err := xml.Unmarshal(body, out); err != nil {
		return nil, errors.New(awserrors.ErrorInvalidInput)
	}
	return out, nil
}
