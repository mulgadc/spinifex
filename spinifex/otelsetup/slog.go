package otelsetup

import (
	"context"
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
