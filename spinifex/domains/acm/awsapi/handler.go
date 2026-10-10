// Package awsapi adapts ACM's AWS JSON 1.1 actions to the ACM domain's
// NATS-backed service contract. Generic gateway routing and authorization stay
// at the ingress boundary.
package awsapi

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/aws/aws-sdk-go/private/protocol/json/jsonutil"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// JSONContentType is the AWS JSON 1.1 content type ACM clients expect.
const JSONContentType = "application/x-amz-json-1.1"

// WriteJSONResponse serialises obj as a 200 AWS JSON 1.1 response using the
// SDK's own jsonutil marshaler, which encodes time.Time as epoch seconds
// (encoding/json emits RFC3339 strings the SDK rejects).
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

// unmarshalIfBody decodes body into out only when non-empty.
func unmarshalIfBody(body []byte, out any) error {
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, out)
}
