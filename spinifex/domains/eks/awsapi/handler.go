// Package awsapi is the HTTP-side glue between awsgw and the EKS handlers.
// EKS speaks AWS REST-JSON 1.1; error envelopes are JSON and Content-Type is
// application/x-amz-json-1.1 throughout.
package awsapi

import (
	"log/slog"
	"net/http"

	"github.com/mulgadc/spinifex/spinifex/ingress/aws/envelope"
)

// WriteJSONError emits the AWS REST-JSON error envelope with the given code,
// message, and HTTP status.
func WriteJSONError(w http.ResponseWriter, code, message string, httpStatus int) {
	body := envelope.JSONBody(code, message)
	w.Header().Set("Content-Type", envelope.JSONContentType)
	if httpStatus == 0 {
		httpStatus = http.StatusInternalServerError
	}
	w.WriteHeader(httpStatus)
	if _, err := w.Write(body); err != nil {
		slog.Error("Failed to write EKS error response", "err", err)
	}
}
