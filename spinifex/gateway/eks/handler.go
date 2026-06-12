// Package gateway_eks is the HTTP-side glue between awsgw and the EKS handlers.
// EKS speaks AWS REST-JSON 1.1; error envelopes are JSON and Content-Type is
// application/x-amz-json-1.1 throughout.
package gateway_eks

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go/private/protocol/json/jsonutil"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// JSONContentType is the AWS REST-JSON 1.1 content type EKS clients expect.
const JSONContentType = "application/x-amz-json-1.1"

// EKSJSONError is the AWS REST-JSON error envelope. SDKs key off __type for
// awserr.Code() and message for awserr.Message().
type EKSJSONError struct {
	Type    string `json:"__type"`
	Message string `json:"message"`
}

// GenerateEKSErrorResponse marshals the AWS REST-JSON error envelope.
// The suffix "Exception" is appended idempotently — codes that already end in
// it (e.g. "ResourceNotFoundException") are left unchanged to avoid the double
// "ExceptionException" the SDK rejects.
func GenerateEKSErrorResponse(code, message string) []byte {
	if !strings.HasSuffix(code, "Exception") {
		code += "Exception"
	}
	body, err := json.Marshal(EKSJSONError{
		Type:    code,
		Message: message,
	})
	if err != nil {
		slog.Error("Failed to marshal EKS error JSON", "code", code, "err", err)
		return fmt.Appendf(nil, `{"__type":"InternalErrorException","message":%q}`, message)
	}
	return body
}

// WriteJSONResponse serializes obj as AWS REST-JSON and writes a 200 response.
// Uses jsonutil.BuildJSON (the SDK's own restjson marshaler) to honour
// locationName tags that encoding/json does not understand.
func WriteJSONResponse(w http.ResponseWriter, obj any) {
	body, err := jsonutil.BuildJSON(obj)
	if err != nil {
		slog.Error("Failed to marshal EKS response JSON", "err", err)
		WriteJSONError(w, awserrors.ErrorInternalError, "failed to marshal response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", JSONContentType)
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		slog.Error("Failed to write EKS response", "err", err)
	}
}

// WriteJSONError emits the AWS REST-JSON error envelope with the given code,
// message, and HTTP status.
func WriteJSONError(w http.ResponseWriter, code, message string, httpStatus int) {
	body := GenerateEKSErrorResponse(code, message)
	w.Header().Set("Content-Type", JSONContentType)
	if httpStatus == 0 {
		httpStatus = http.StatusInternalServerError
	}
	w.WriteHeader(httpStatus)
	if _, err := w.Write(body); err != nil {
		slog.Error("Failed to write EKS error response", "err", err)
	}
}

// WriteErrorFromCode looks up an awserrors code and writes the JSON error
// envelope. Falls back to 500 InternalError for unknown codes.
func WriteErrorFromCode(w http.ResponseWriter, code string) {
	msg, ok := awserrors.ErrorLookup[code]
	if !ok {
		slog.Warn("Unknown EKS error code", "code", code)
		WriteJSONError(w, awserrors.ErrorInternalError, "Internal error", http.StatusInternalServerError)
		return
	}
	httpStatus := msg.HTTPCode
	if httpStatus == 0 {
		httpStatus = http.StatusInternalServerError
	}
	WriteJSONError(w, code, msg.Message, httpStatus)
}

// ParseJSONBody decodes the request body into an aws-sdk-go input struct.
// Empty bodies (valid for GET/DELETE) return a zero-valued struct.
func ParseJSONBody[T any](r *http.Request) (*T, error) {
	out := new(T)
	if r.Body == nil {
		return out, nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(body) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	return out, nil
}
