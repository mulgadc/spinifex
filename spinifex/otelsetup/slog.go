package otelsetup

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel/trace"
)

// SetDefaultJSONLogger installs the process-wide slog default: JSON to
// stdout (journald) at the given level, with trace_id/span_id stamping.
func SetDefaultJSONLogger(level slog.Level) {
	slog.SetDefault(slog.New(NewSlogHandler(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	}))))
}

var _ slog.Handler = (*traceHandler)(nil)

// traceHandler stamps trace_id/span_id from the record's context onto every
// log line so any log can be pivoted to its trace in the backend.
type traceHandler struct {
	inner slog.Handler
}

// NewSlogHandler wraps inner so records logged with a context carrying an
// active span gain trace_id and span_id attributes. Records without a span
// pass through unchanged.
func NewSlogHandler(inner slog.Handler) slog.Handler {
	return &traceHandler{inner: inner}
}

func (h *traceHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.inner.Handle(ctx, r)
}

func (h *traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &traceHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h *traceHandler) WithGroup(name string) slog.Handler {
	return &traceHandler{inner: h.inner.WithGroup(name)}
}

var _ slog.Handler = (*fanoutHandler)(nil)

// fanoutHandler writes every record to all inner handlers, e.g. so OTLP
// export is additive to the existing stdout/journald handler rather than
// replacing it.
type fanoutHandler struct {
	handlers []slog.Handler
}

// newFanoutHandler returns a handler that fans out to all of handlers.
func newFanoutHandler(handlers ...slog.Handler) slog.Handler {
	return &fanoutHandler{handlers: handlers}
}

func (h *fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, inner := range h.handlers {
		if inner.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (h *fanoutHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs error
	for _, inner := range h.handlers {
		if inner.Enabled(ctx, r.Level) {
			errs = errors.Join(errs, inner.Handle(ctx, r.Clone()))
		}
	}
	return errs
}

func (h *fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make([]slog.Handler, len(h.handlers))
	for i, inner := range h.handlers {
		next[i] = inner.WithAttrs(attrs)
	}
	return &fanoutHandler{handlers: next}
}

func (h *fanoutHandler) WithGroup(name string) slog.Handler {
	next := make([]slog.Handler, len(h.handlers))
	for i, inner := range h.handlers {
		next[i] = inner.WithGroup(name)
	}
	return &fanoutHandler{handlers: next}
}

// addFanoutHandler rewraps the process slog default so records also reach
// extra, in addition to whatever handler is already installed (e.g. the
// stdout/journald handler from SetDefaultJSONLogger). Used by Init to bolt
// the OTLP bridge onto an already-configured default logger.
func addFanoutHandler(extra slog.Handler) {
	slog.SetDefault(slog.New(newFanoutHandler(slog.Default().Handler(), extra)))
}
