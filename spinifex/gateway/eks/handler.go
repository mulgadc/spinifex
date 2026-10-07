// Package gateway_eks is the HTTP-side glue between awsgw and the EKS handlers.
// EKS speaks AWS REST-JSON 1.1; error envelopes are JSON and Content-Type is
// application/x-amz-json-1.1 throughout.
package gateway_eks

import (
	"log/slog"
	"net/http"

	"github.com/aws/aws-sdk-go/private/protocol/json/jsonutil"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/envelope"
)

// JSONContentType is the AWS REST-JSON 1.1 content type EKS clients expect.
const JSONContentType = "application/x-amz-json-1.1"

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
	body := envelope.JSONBody(code, message)
	w.Header().Set("Content-Type", JSONContentType)
	if httpStatus == 0 {
		httpStatus = http.StatusInternalServerError
	}
	w.WriteHeader(httpStatus)
	if _, err := w.Write(body); err != nil {
		slog.Error("Failed to write EKS error response", "err", err)
	}
}
