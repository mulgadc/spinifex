package gateway

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/mulgadc/spinifex/spinifex/utils"
)

// ctxAudit carries the per-request audit record. It is a pointer, unlike every
// other value in this file's sibling context keys, because the request logger
// is the OUTERMOST middleware and a context derived downstream never propagates
// back up to it. The record is the only way the logger sees who called.
const ctxAudit contextKey = "gateway.audit"

// requestAudit is what the gateway knows about a request by the time it answers
// it: who called, what they asked for, and how it was decided. It holds an
// access key id and an error code, never a secret key, a session token or an
// Authorization header — nothing here may carry a credential.
type requestAudit struct {
	mu            sync.Mutex
	clientIP      string
	accessKeyID   string
	accountID     string
	region        string
	service       string
	action        string
	principalType string
	authError     string
}

// requestAuditMiddleware attaches an audit record to every request. It is
// registered unconditionally: span enrichment must not depend on whether access
// logging happens to be enabled.
func requestAuditMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		audit := &requestAudit{clientIP: utils.ClientIP(r.RemoteAddr)}
		ctx := context.WithValue(r.Context(), ctxAudit, audit)
		next.ServeHTTP(w, r.WithContext(ctx))
		audit.annotate(ctx)
	})
}

// auditFrom returns the request's audit record, or nil when there is none. Every
// method below is nil-safe, so a handler called directly by a test behaves
// exactly as it did before this record existed.
func auditFrom(ctx context.Context) *requestAudit {
	audit, _ := ctx.Value(ctxAudit).(*requestAudit)
	return audit
}

// setIdentity records the authenticated caller. Called once, where the auth
// middleware already puts the same values into the context.
func (a *requestAudit) setIdentity(accessKeyID, accountID, region, service, principalType string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.accessKeyID = accessKeyID
	a.accountID = accountID
	a.region = region
	a.service = service
	a.principalType = principalType
}

// setAction records the resolved AWS action. Query-protocol services resolve it
// before dispatch, REST-JSON services once their dispatcher has run.
func (a *requestAudit) setAction(service, action string) {
	if a == nil || action == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.action = action
	if service != "" {
		a.service = service
	}
}

// setAuthError records the AWS error code a request was rejected with. Every
// auth rejection funnels through writeSigV4Error, including a rate-limit
// lockout, so recording it there covers all of them.
func (a *requestAudit) setAuthError(errorCode string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.authError = errorCode
}

// setAccessKeyID records the key a request presented even when authentication
// failed, so a rejected caller is still identifiable.
func (a *requestAudit) setAccessKeyID(accessKeyID string) {
	if a == nil || accessKeyID == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.accessKeyID = accessKeyID
}

// fields returns the non-empty audit values as alternating key/value pairs for
// slog. Empty values are omitted rather than logged blank, so an unauthenticated
// request does not carry a row of empty identity fields.
func (a *requestAudit) fields() []any {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	var out []any
	for _, kv := range a.pairs() {
		if kv.value != "" {
			out = append(out, kv.key, kv.value)
		}
	}
	return out
}

// annotate copies the audit values onto the server span, so a trace search for
// failing requests answers from where, doing what and rejected why without
// having to join against the log stream.
func (a *requestAudit) annotate(ctx context.Context) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	for _, kv := range a.pairs() {
		if kv.value != "" {
			span.SetAttributes(attribute.String(kv.spanKey, kv.value))
		}
	}
}

type auditField struct {
	key     string
	spanKey string
	value   string
}

// pairs names each field once for both sinks. Callers hold a.mu.
func (a *requestAudit) pairs() []auditField {
	return []auditField{
		{"sourceIP", "client.address", a.clientIP},
		{"accessKeyID", "aws.access_key_id", a.accessKeyID},
		{"accountID", "aws.account_id", a.accountID},
		{"region", "aws.region", a.region},
		{"service", "aws.service", a.service},
		{"action", "aws.action", a.action},
		{"principalType", "aws.principal_type", a.principalType},
		{"authError", "aws.auth_error", a.authError},
	}
}

// logRequest emits the access log line for a finished request, carrying the
// audit fields alongside the HTTP result.
func logRequest(r *http.Request, status int, duration time.Duration) {
	attrs := []any{"method", r.Method, "path", r.URL.Path, "status", status, "duration", duration}
	slog.InfoContext(r.Context(), "request", append(attrs, auditFrom(r.Context()).fields()...)...)
}
