package pipeline

import (
	"context"
	"encoding/json"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/reqctx"
	"log/slog"
	"net/http"
	"uuid"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// AdminSurface names the registration the private /admin surface dispatches
// to and how the request ID reaches it. The composing role supplies both, so
// the gateway names no service.
type AdminSurface struct {
	Service       string
	WithRequestID func(ctx context.Context, requestID string) context.Context
}

// Admin_Request serves the private /admin/<Method> surface. The route stays
// here so it keeps its place ahead of the service selector; the work is the
// spinifex registration's, dispatched exactly as a Query request is. Only the
// error rendering differs: this surface answers in JSON with a request ID.
func (gw *Config) Admin_Request(w http.ResponseWriter, r *http.Request) {
	requestID := uuid.NewV4().String()
	w.Header().Set("X-Amzn-Requestid", requestID)
	if gw.Admin.WithRequestID != nil {
		r = r.WithContext(gw.Admin.WithRequestID(r.Context(), requestID))
	}

	entry, ok := gw.registered(gw.Admin.Service)
	if !ok {
		slog.Error("Admin: spinifex service is not registered", "requestId", requestID)
		gw.writeAdminError(w, requestID, awserrors.ErrorInternalError, "")
		return
	}

	action := reqctx.Action(r.Context())
	if err := gw.dispatchRegistered(entry, w, r); err != nil {
		slog.Log(r.Context(), serviceErrorLevel(gw.Admin.Service, err), "Service request error",
			"service", gw.Admin.Service, "action", action, "requestId", requestID, "error", err)
		gw.writeAdminErrorFor(w, requestID, err)
		return
	}
	slog.Info("Service request completed", "service", gw.Admin.Service, "action", action, "requestId", requestID)
}

// adminErrorBody is the JSON error envelope for /admin. This surface is not an
// AWS API, so it does not use the EC2 or IAM XML envelopes.
type adminErrorBody struct {
	Error     adminErrorDetail `json:"error"`
	RequestID string           `json:"requestId"`
}

type adminErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeAdminErrorFor resolves an error to its code and any message its call
// site attached, so a wrapped error renders as its own code instead of falling
// through to InternalError.
func (gw *Config) writeAdminErrorFor(w http.ResponseWriter, requestID string, err error) {
	code, message, ok := awserrors.ResolveErrorDetail(err)
	if !ok {
		// The cause reaches neither the wire nor the envelope's own unmapped-code
		// log, so this is the only place it is recorded.
		slog.Error("Admin: unresolvable error", "requestId", requestID, "err", err)
		code = awserrors.ErrorInternalError
	}
	gw.writeAdminError(w, requestID, code, message)
}

// writeAdminError writes the JSON error envelope. message overrides the
// registry text when a specific cause is safe to disclose; a policy denial
// names principal, action and resource, all of them caller-derivable.
func (gw *Config) writeAdminError(w http.ResponseWriter, requestID, code, message string) {
	detail, ok := awserrors.ErrorLookup[code]
	if !ok {
		slog.Error("Admin: unmapped error code", "code", code)
		code = awserrors.ErrorInternalError
		detail = awserrors.ErrorLookup[awserrors.ErrorInternalError]
	}
	if message == "" {
		message = detail.Message
	}

	body, err := json.Marshal(adminErrorBody{
		Error:     adminErrorDetail{Code: code, Message: message},
		RequestID: requestID,
	})
	if err != nil {
		slog.Error("Admin: failed to marshal error body", "err", err)
		http.Error(w, `{"error":{"code":"InternalError"}}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(detail.HTTPCode)
	if _, err := w.Write(body); err != nil {
		slog.Error("Admin: failed to write error response", "err", err)
	}
}
