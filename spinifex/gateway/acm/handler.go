// Package gateway_acm is the HTTP-side glue between awsgw and the acm handlers
// package. ACM speaks AWS JSON 1.1 (X-Amz-Target dispatch, Content-Type
// application/x-amz-json-1.1), so responses are JSON throughout. Error
// envelopes are emitted centrally by the shared gateway ErrorHandler.
package gateway_acm

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/aws/aws-sdk-go/private/protocol/json/jsonutil"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// JSONContentType is the AWS JSON 1.1 content type ACM clients expect.
const JSONContentType = "application/x-amz-json-1.1"

// WriteJSONResponse serialises obj as a 200 AWS JSON 1.1 response. obj is an
// aws-sdk-go *Output struct whose time.Time fields must serialise as epoch
// seconds (the AWS JSON 1.1 timestamp wire format); encoding/json would emit
// RFC3339 strings the AWS SDKs reject with "Expected real number". jsonutil
// is the marshaler the SDK itself uses for this protocol.
func WriteJSONResponse(w http.ResponseWriter, obj any) {
	body, err := jsonutil.BuildJSON(obj)
	if err != nil {
		slog.Error("ACM: failed to marshal response JSON", "err", err)
		http.Error(w, awserrors.ErrorInternalError, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", JSONContentType)
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		slog.Error("ACM: failed to write response", "err", err)
	}
}

// unmarshalIfBody decodes body into out only when body is non-empty, so
// actions with an optional body share one helper.
func unmarshalIfBody(body []byte, out any) error {
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, out)
}
