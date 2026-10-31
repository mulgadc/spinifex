package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	operatorv1 "github.com/mulgadc/spinifex/contracts/operator/v1"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	ingresshttp "github.com/mulgadc/spinifex/spinifex/ingress/http"
)

type ctxKey string

const ctxRequestID ctxKey = "admin.requestID"

// WithRequestID carries the /admin request ID to handlers that log under it.
// It correlates log records only and authorises nothing.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxRequestID, id)
}

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxRequestID).(string)
	return id
}

// actorName is the name the caller goes by: the session name for a role
// session and the user name for every other principal.
func actorName(a dispatch.Actor) string {
	if a.PrincipalType == "assumed-role" {
		return a.SessionName
	}
	return a.UserName
}

// RunMethod runs one /admin method against the request body and returns the
// value to marshal as the JSON response. caller is the verified caller.
func (d Deps) RunMethod(ctx context.Context, method string, caller dispatch.Actor, body []byte) (any, error) {
	switch method {
	case "CreateAccount":
		return d.createAccount(ctx, body)
	case "DeleteAccount":
		return d.deleteAccount(ctx, body)
	case "DescribeAccountDeletion":
		return d.describeAccountDeletion(ctx, body)
	case "ListAccounts":
		return d.listAccounts(ctx, body)
	case "GetAccountQuota":
		return d.getAccountQuota(ctx, body)
	case "PutAccountQuota":
		return d.putAccountQuota(ctx, caller, body)
	}
	// The caller's method set is gated by the contract list, so this is
	// unreachable; failing closed keeps a name added there without a case from
	// silently returning 200.
	return nil, errors.New(awserrors.ErrorInvalidAction)
}

// pathPrefix is the private admin surface's URL prefix.
const pathPrefix = "/admin/"

// maxBodyBytes caps the JSON request body. The largest legitimate request is a
// few hundred bytes; the cap stops a mistaken caller making the gateway buffer
// megabytes.
const maxBodyBytes = 8 << 10

// clusterUnavailableMsg is the 503 body when NATS is disconnected.
const clusterUnavailableMsg = "cluster unavailable: NATS disconnected — check daemon /local/status"

var methods = func() map[string]bool {
	m := make(map[string]bool)
	for _, name := range operatorv1.AdminMethodNames() {
		m[name] = true
	}
	return m
}()

// PathMethod extracts the method from an /admin/<Method> path. It reports false
// for anything else, including a nested path or a method outside the contract
// list, so only the exact shape the router serves is labelled.
func PathMethod(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, pathPrefix)
	if !ok || rest == "" || strings.Contains(rest, "/") || !methods[rest] {
		return "", false
	}
	return rest, true
}

// isAdminPath reports whether the path is under the /admin surface, whether or
// not the method is a known one.
func isAdminPath(path string) bool {
	return strings.HasPrefix(path, pathPrefix)
}

// dispatchREST serves one /admin/<Method> request. The order is fixed: verb and
// method first, then the fail-closed identity gates, then the cluster check
// and the body cap. A denial is the same AccessDenied at every gate so a caller
// cannot tell which one rejected it.
func (d Deps) dispatchREST(w http.ResponseWriter, inv dispatch.Invocation) error {
	r := inv.Request
	if r.Method != http.MethodPost {
		return errors.New(awserrors.ErrorMethodNotAllowed)
	}
	method := strings.TrimPrefix(r.URL.Path, pathPrefix)
	if !methods[method] {
		slog.DebugContext(r.Context(), "Admin: unknown method", "method", method)
		return errors.New(awserrors.ErrorInvalidAction)
	}

	caller, _ := inv.Actor()
	if err := d.authorizeREST(inv, method, caller); err != nil {
		return err
	}

	if d.NATS == nil || !d.NATS.IsConnected() {
		return awserrors.Errorf(awserrors.ErrorServiceUnavailable, "%s", clusterUnavailableMsg)
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		slog.ErrorContext(r.Context(), "Admin: failed to read request body", "method", method, "err", err)
		return errors.New(awserrors.ErrorInternalError)
	}
	if len(body) > maxBodyBytes {
		return awserrors.Errorf(awserrors.ErrorInvalidRequest, "request body exceeds 8 KiB")
	}

	output, err := d.RunMethod(r.Context(), method, caller, body)
	if err != nil {
		return err
	}
	jsonOutput, err := json.Marshal(output)
	if err != nil {
		slog.ErrorContext(r.Context(), "Admin: failed to marshal response", "method", method, "err", err)
		return errors.New(awserrors.ErrorInternalError)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(jsonOutput); err != nil {
		slog.ErrorContext(r.Context(), "Admin: failed to write response", "method", method, "err", err)
	}
	return nil
}

// authorizeREST runs the fail-closed gates. Each returns the same AccessDenied;
// the log line names the gate for operators.
func (d Deps) authorizeREST(inv dispatch.Invocation, method string, caller dispatch.Actor) error {
	r := inv.Request
	deny := func(reason string) error {
		slog.WarnContext(r.Context(), "Admin: access denied", "method", method, "reason", reason,
			"service", inv.Service, "accountID", caller.AccountID, "identity", actorName(caller),
			"principalType", caller.PrincipalType, "sourceIP", ingresshttp.ClientIP(r.RemoteAddr))
		return errors.New(awserrors.ErrorAccessDenied)
	}

	// A key signed for another service reaching this path means the caller is
	// probing, not calling.
	if inv.Service != ServiceName {
		return deny("credential scope is not spinifex")
	}
	if caller.AccountID != awsidentifiers.DefaultAccountID() {
		return deny("not the super-admin account")
	}
	// Long-lived IAM users only, which excludes assumed-role sessions outright.
	if caller.PrincipalType != "user" {
		return deny("principal is not an IAM user")
	}
	if actorName(caller) == "" {
		return deny("no identity in auth context")
	}
	return inv.Authorize(ServiceName, method, []string{"*"}, nil)
}
