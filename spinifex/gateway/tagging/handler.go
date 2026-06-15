// Package gateway_tagging is the HTTP-side glue between awsgw and the Resource
// Groups Tagging API. Speaks AWS JSON 1.1; GetResources aggregates the elbv2
// and ec2 tag stores over NATS. Errors are handled by the shared gateway ErrorHandler.
package gateway_tagging

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/aws/aws-sdk-go/private/protocol/json/jsonutil"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// JSONContentType is the AWS JSON 1.1 content type tagging clients expect.
const JSONContentType = "application/x-amz-json-1.1"

// WriteJSONResponse serialises obj as a 200 AWS JSON 1.1 response using the
// SDK's own marshaler (epoch-seconds timestamps).
func WriteJSONResponse(w http.ResponseWriter, obj any) {
	body, err := jsonutil.BuildJSON(obj)
	if err != nil {
		slog.Error("tagging: failed to marshal response JSON", "err", err)
		http.Error(w, awserrors.ErrorInternalError, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", JSONContentType)
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		slog.Error("tagging: failed to write response", "err", err)
	}
}

// unmarshalIfBody decodes body into out only when body is non-empty.
func unmarshalIfBody(body []byte, out any) error {
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, out)
}
