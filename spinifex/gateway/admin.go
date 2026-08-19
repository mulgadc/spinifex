package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// adminMethods lists the callable /admin/<Method> names. A method absent here
// is rejected before any parsing, so adding a handler is a deliberate act
// rather than a side effect of adding a function.
var adminMethods = map[string]bool{
	"CreateAccount":           true,
	"DeleteAccount":           true,
	"DescribeAccountDeletion": true,
	"GetAccountQuota":         true,
	"ListAccounts":            true,
	"PutAccountQuota":         true,
}

// AdminMethodNames returns the callable /admin/<Method> names in sorted order.
// Tooling that mints a credential grants these by name, so the list has to come
// from the router rather than from a copy that can drift out of step with it.
func AdminMethodNames() []string {
	names := make([]string, 0, len(adminMethods))
	for name := range adminMethods {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// adminPathPrefix is the private admin surface's URL prefix. It is matched
// before routing so the auth middleware can label the request by method.
const adminPathPrefix = "/admin/"

// adminPathMethod extracts the method from an /admin/<Method> path. It reports
// false for anything else, including a nested path, so only the exact shape the
// router serves is labelled.
func adminPathMethod(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, adminPathPrefix)
	if !ok || rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	if !adminMethods[rest] {
		return "", false
	}
	return rest, true
}

// adminMaxBodyBytes caps the JSON request body. The largest legitimate request
// is a few hundred bytes; the cap exists so an unauthenticated-shaped mistake
// cannot make the gateway buffer megabytes.
const adminMaxBodyBytes = 8 << 10

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

// Admin_Request dispatches the private /admin/<Method> surface: super-admin
// only, JSON in and JSON out, SigV4-authenticated by the shared middleware.
//
// Authorization is fail-closed at every step and every denial returns the same
// AccessDenied, so a caller cannot probe which gate rejected it.
func (gw *GatewayConfig) Admin_Request(w http.ResponseWriter, r *http.Request) {
	requestID := uuid.NewString()
	w.Header().Set("X-Amzn-Requestid", requestID)

	method := chi.URLParam(r, "method")
	ctx := r.Context()

	// One handler for every verb rather than a POST route plus a catch-all:
	// chi resolves the two to the same pattern, and the catch-all wins.
	if r.Method != http.MethodPost {
		gw.writeAdminError(w, requestID, awserrors.ErrorMethodNotAllowed, "")
		return
	}

	if !adminMethods[method] {
		slog.Debug("Admin: unknown method", "method", method)
		gw.writeAdminError(w, requestID, awserrors.ErrorInvalidAction, "")
		return
	}

	if err := gw.authorizeAdmin(r, method); err != nil {
		gw.writeAdminError(w, requestID, err.Error(), "")
		return
	}

	if gw.NATSConn == nil || !gw.NATSConn.IsConnected() {
		gw.writeAdminError(w, requestID, awserrors.ErrorServiceUnavailable, clusterUnavailableMsg)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, adminMaxBodyBytes+1))
	if err != nil {
		slog.Error("Admin: failed to read request body", "method", method, "err", err)
		gw.writeAdminError(w, requestID, awserrors.ErrorInternalError, "")
		return
	}
	if len(body) > adminMaxBodyBytes {
		gw.writeAdminError(w, requestID, awserrors.ErrorInvalidRequest, "request body exceeds 8 KiB")
		return
	}

	var output any
	switch method {
	case "CreateAccount":
		output, err = gw.adminCreateAccount(ctx, body)
	case "DeleteAccount":
		output, err = gw.adminDeleteAccount(ctx, body)
	case "DescribeAccountDeletion":
		output, err = gw.adminDescribeAccountDeletion(ctx, body)
	case "ListAccounts":
		output, err = gw.adminListAccounts(ctx, body)
	case "GetAccountQuota":
		output, err = gw.adminGetAccountQuota(ctx, body)
	case "PutAccountQuota":
		output, err = gw.adminPutAccountQuota(ctx, body)
	default:
		// Unreachable: adminMethods gates the switch. Fail closed anyway so
		// adding a name to the map without a case cannot silently return 200.
		err = errors.New(awserrors.ErrorInvalidAction)
	}
	if err != nil {
		gw.writeAdminError(w, requestID, err.Error(), "")
		return
	}

	jsonOutput, err := json.Marshal(output)
	if err != nil {
		slog.Error("Admin: failed to marshal response", "method", method, "err", err)
		gw.writeAdminError(w, requestID, awserrors.ErrorInternalError, "")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(jsonOutput); err != nil {
		slog.Error("Admin: failed to write response", "method", method, "err", err)
	}
}

// authorizeAdmin runs the five fail-closed gates. Each returns the same
// AccessDenied so the response cannot be used to enumerate which condition the
// caller failed; the log line names it for operators.
func (gw *GatewayConfig) authorizeAdmin(r *http.Request, method string) error {
	ctx := r.Context()
	service, _ := ctx.Value(ctxService).(string)
	accountID, _ := ctx.Value(ctxAccountID).(string)
	identity, _ := ctx.Value(ctxIdentity).(string)
	principalType, _ := ctx.Value(ctxPrincipalType).(string)

	deny := func(reason string) error {
		slog.Warn("Admin: access denied", "method", method, "reason", reason,
			"service", service, "accountID", accountID, "identity", identity,
			"principalType", principalType, "sourceIP", utils.ClientIP(r.RemoteAddr))
		return errors.New(awserrors.ErrorAccessDenied)
	}

	// The credential scope must be spinifex: a key signed for ec2 reaching this
	// path means the caller is probing, not calling.
	if service != "spinifex" {
		return deny("credential scope is not spinifex")
	}
	if accountID != admin.DefaultAccountID() {
		return deny("not the super-admin account")
	}
	// Long-lived IAM users only. This excludes assumed-role sessions outright,
	// so a role session cannot reach account creation even from this account.
	if principalType != principalTypeUser {
		return deny("principal is not an IAM user")
	}
	if identity == "" {
		return deny("no identity in auth context")
	}

	// Defence in depth and the actual grant: the principal must hold
	// spinifex:<Method>. checkPolicy fails closed on a nil IAM service, an
	// unauthenticated request and an unresolvable principal.
	return gw.checkPolicy(r, "spinifex", method)
}

// writeAdminError writes the JSON error envelope. message overrides the
// registry text when a specific cause is safe to disclose; it must never carry
// a parameter value, only a parameter name.
func (gw *GatewayConfig) writeAdminError(w http.ResponseWriter, requestID, code, message string) {
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
