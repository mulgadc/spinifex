// Package gateway_eks holds the HTTP-side glue between awsgw and the EKS
// handlers package. EKS speaks AWS REST-JSON 1.1 (not the query+XML protocol
// that EC2 / ELBv2 use), so error envelopes here are JSON and the
// Content-Type is application/x-amz-json-1.1 throughout.
package gateway_eks

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/aws/aws-sdk-go/private/protocol/json/jsonutil"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// JSONContentType is the AWS REST-JSON 1.1 content type EKS clients expect on
// every request and response.
const JSONContentType = "application/x-amz-json-1.1"

// EKSJSONError is the AWS REST-JSON error envelope. AWS SDKs key off
// __type to populate awserr.Code() and message to populate awserr.Message().
type EKSJSONError struct {
	Type    string `json:"__type"`
	Message string `json:"message"`
}

// GenerateEKSErrorResponse marshals the AWS REST-JSON error envelope.
// The trailing "Exception" suffix matches the convention AWS uses
// (e.g. ResourceNotFoundException, InvalidParameterException).
func GenerateEKSErrorResponse(code, message string) []byte {
	body, err := json.Marshal(EKSJSONError{
		Type:    code + "Exception",
		Message: message,
	})
	if err != nil {
		slog.Error("Failed to marshal EKS error JSON", "code", code, "err", err)
		return fmt.Appendf(nil, `{"__type":"InternalErrorException","message":%q}`, message)
	}
	return body
}

// WriteJSONResponse serializes obj as AWS REST-JSON and writes it as a 200
// response. aws-sdk-go *Output structs tag their fields with locationName (the
// restjson wire name) and carry no json: tags, so encoding/json would emit Go
// PascalCase keys the AWS SDK cannot parse. jsonutil.BuildJSON is the same
// marshaler the SDK uses for restjson bodies and honors locationName.
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

// WriteJSONError emits the AWS REST-JSON error envelope with the given AWS
// error code, message, and HTTP status. The Content-Type matches what real
// EKS returns.
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

// WriteErrorFromCode looks up an awserrors code, maps it to its HTTP status
// and message, and writes the JSON envelope. Falls back to a 500 InternalError
// when the code isn't registered.
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

// ParseJSONBody is the generic helper every per-action handler uses to decode
// the request body into an aws-sdk-go input struct. Empty bodies are valid
// for GET / DELETE actions; the returned struct will simply have all
// zero-valued fields.
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
