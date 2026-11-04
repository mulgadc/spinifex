// HTTP-side glue between awsgw and the Ochre (bedrock / bedrock-runtime)
// handlers. Both speak AWS REST-JSON 1.1; error envelopes are JSON and
// Content-Type is application/x-amz-json-1.1 throughout.

package ochre

import (
	"log/slog"
	"net/http"
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
