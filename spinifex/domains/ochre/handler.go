// HTTP-side glue between awsgw and the Ochre (bedrock / bedrock-runtime)
// handlers. Both speak AWS REST-JSON 1.1; error envelopes are JSON and
// Content-Type is application/x-amz-json-1.1 throughout.

package ochre

import (
	"log/slog"
	"net/http"

	"github.com/mulgadc/spinifex/spinifex/ingress/aws/envelope"
)

// WriteRawResponse writes body verbatim with contentType and a 200 status,
// bypassing JSON marshaling. Used by InvokeModel, whose response is the
// provider-native body rather than a struct.
func WriteRawResponse(w http.ResponseWriter, body []byte, contentType string) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		slog.Error("Failed to write bedrock raw response", "err", err)
	}
}

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
		slog.Error("Failed to write bedrock error response", "err", err)
	}
}
