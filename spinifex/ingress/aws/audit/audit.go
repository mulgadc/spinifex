// Package audit records who called the AWS gateway, what they asked for and how
// the request was decided, for the access log and the server span.
package audit

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
	ingresshttp "github.com/mulgadc/spinifex/spinifex/ingress/http"
)

// ctxAudit carries the per-request audit record. It is a pointer, unlike the
// gateway's other context keys, because the request logger is the OUTERMOST
// middleware and a context derived downstream never propagates back up to it.
const ctxAudit contextKey = "gateway.audit"

type contextKey string

// Record is what the gateway knows about a request by the time it answers
// it: who called, what they asked for, and how it was decided. It holds an
// access key id and an error code, never a secret key, a session token or an
// Authorization header — nothing here may carry a credential.
type Record struct {
	mu            sync.Mutex
	clientIP      string
	requestID     string
	accessKeyID   string
	accountID     string
	region        string
	service       string
	action        string
	principalType string
	authError     string
	awsRequestID  string
}

// Middleware attaches an audit record to every request. It is
// registered unconditionally: span enrichment must not depend on whether access
// logging happens to be enabled.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		audit := &Record{clientIP: ingresshttp.RequestClientIP(r), requestID: requestIDFrom(r)}
		ctx := context.WithValue(r.Context(), ctxAudit, audit)
		next.ServeHTTP(w, r.WithContext(ctx))
		audit.annotate(ctx)
	})
}

// requestIDPattern is what a correlation ID may look like. nginx's $request_id is
// 32 hex characters; anything else a client sends is dropped rather than logged.
var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// requestIDFrom returns the X-Request-ID the edge stamped, so an awsgw line can
// be joined to the edge access-log line that produced it.
func requestIDFrom(r *http.Request) string {
	id := r.Header.Get("X-Request-ID")
	if !requestIDPattern.MatchString(id) {
		return ""
	}
	return id
}

// From returns the request's audit record, or nil when there is none. Every
// method below is nil-safe, so a handler called directly by a test behaves
// exactly as it did before this record existed.
func From(ctx context.Context) *Record {
	audit, _ := ctx.Value(ctxAudit).(*Record)
	return audit
}

// SetIdentity records the authenticated caller. Called once, where the auth
// middleware already puts the same values into the context.
func (a *Record) SetIdentity(accessKeyID, accountID, region, service, principalType string) {
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

// SetAction records the resolved AWS action. Query-protocol services resolve it
// before dispatch, REST-JSON services once their dispatcher has run.
func (a *Record) SetAction(service, action string) {
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

// SetAuthError records the AWS error code a request was rejected with. Every
// auth rejection funnels through writeSigV4Error, including a rate-limit
// lockout, so recording it there covers all of them.
func (a *Record) SetAuthError(errorCode string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.authError = errorCode
}

// SetAWSRequestID records the request ID an error response was answered with,
// so a client-reported x-amzn-RequestId joins to this request's log line.
func (a *Record) SetAWSRequestID(id string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.awsRequestID = id
}

// SetAccessKeyID records the key a request presented even when authentication
// failed, so a rejected caller is still identifiable.
func (a *Record) SetAccessKeyID(accessKeyID string) {
	if a == nil || accessKeyID == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.accessKeyID = accessKeyID
}

// Fields returns the non-empty audit values as alternating key/value pairs for
// slog. Empty values are omitted rather than logged blank, so an unauthenticated
// request does not carry a row of empty identity fields.
func (a *Record) Fields() []any {
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
func (a *Record) annotate(ctx context.Context) {
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
func (a *Record) pairs() []auditField {
	return []auditField{
		{"sourceIP", "client.address", a.clientIP},
		{"accessKeyID", "aws.access_key_id", a.accessKeyID},
		{"accountID", "aws.account_id", a.accountID},
		{"region", "aws.region", a.region},
		{"service", "aws.service", a.service},
		{"action", "aws.action", a.action},
		{"principalType", "aws.principal_type", a.principalType},
		{"authError", "aws.auth_error", a.authError},
		{"requestID", "http.request.header.x-request-id", a.requestID},
		{"awsRequestID", "aws.request_id", a.awsRequestID},
	}
}

// logRequest emits the access log line for a finished request, carrying the
// audit fields alongside the HTTP result.
func logRequest(r *http.Request, status int, duration time.Duration) {
	attrs := []any{"method", r.Method, "path", r.URL.Path, "status", status, "duration_ms", otelsetup.Millis(duration)}
	slog.InfoContext(r.Context(), "request", append(attrs, From(r.Context()).Fields()...)...)
}

// RequestLogger is a middleware that logs each request via slog. The audit
// record carries the caller and the auth verdict onto the same line, so a
// failing request is answerable without joining it to a separate auth log.
func RequestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		logRequest(r, ww.Status(), time.Since(start))
	})
}
