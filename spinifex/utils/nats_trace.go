package utils

import (
	"context"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const natsTracerName = "github.com/mulgadc/spinifex/spinifex/utils"

var _ propagation.TextMapCarrier = (*natsHeaderCarrier)(nil)

// natsHeaderCarrier adapts nats.Header to the OTel TextMapCarrier so the
// W3C traceparent rides NATS message headers beside X-Account-ID.
type natsHeaderCarrier nats.Header

func (c natsHeaderCarrier) Get(key string) string { return nats.Header(c).Get(key) }
func (c natsHeaderCarrier) Set(key, value string) { nats.Header(c).Set(key, value) }
func (c natsHeaderCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}

// InjectTraceContext writes ctx's span context into hdr (traceparent/tracestate).
func InjectTraceContext(ctx context.Context, hdr nats.Header) {
	otel.GetTextMapPropagator().Inject(ctx, natsHeaderCarrier(hdr))
}

// ExtractTraceContext returns ctx extended with the remote span context
// carried in msg's headers, if any.
func ExtractTraceContext(ctx context.Context, msg *nats.Msg) context.Context {
	if msg == nil || msg.Header == nil {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx, natsHeaderCarrier(msg.Header))
}

// StartConsumerSpan extracts the producer's trace context from msg and opens
// a consumer span for the subject. Callers must End() the returned span and
// should pass the context into handler logic so logs correlate.
func StartConsumerSpan(msg *nats.Msg) (context.Context, trace.Span) {
	ctx := ExtractTraceContext(context.Background(), msg)
	return otel.Tracer(natsTracerName).Start(ctx, "NATS "+msg.Subject,
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("messaging.system", "nats"),
			attribute.String("messaging.destination.name", msg.Subject),
		))
}

// startProducerSpan opens a client span for an outbound request on subject.
func startProducerSpan(ctx context.Context, subject string) (context.Context, trace.Span) {
	return otel.Tracer(natsTracerName).Start(ctx, "NATS "+subject,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("messaging.system", "nats"),
			attribute.String("messaging.destination.name", subject),
		))
}

// endSpanWithError records err (if any) and ends the span.
func endSpanWithError(span trace.Span, err error) {
	MarkSpanError(span, err)
	span.End()
}

// MarkSpanError records err (if any) on span and sets error status without
// ending it. Use when the span's lifetime outlives the failure site.
func MarkSpanError(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
}
