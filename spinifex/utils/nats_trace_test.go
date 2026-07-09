package utils

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

type traceEchoRequest struct {
	Name string `json:"name"`
}

type traceEchoResponse struct {
	Name string `json:"name"`
}

// TestNATSTracePropagation proves parent/child span linkage across a real
// publish->subscribe round trip, with the AccountID header intact beside it.
func TestNATSTracePropagation(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	prevTP := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))
	defer otel.SetTracerProvider(prevTP)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	ns := startTestNATSServer(t)
	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Close()

	var gotAccount string
	var consumerSC trace.SpanContext
	_, err = nc.Subscribe("test.trace.echo", func(msg *nats.Msg) {
		gotAccount = AccountIDFromMsg(msg)
		ServeNATSRequestCtx(msg, func(ctx context.Context, in *traceEchoRequest) (*traceEchoResponse, error) {
			consumerSC = trace.SpanContextFromContext(ctx)
			return &traceEchoResponse{Name: in.Name}, nil
		})
	})
	require.NoError(t, err)

	ctx, parent := otel.Tracer("test").Start(context.Background(), "parent-op")
	out, err := NATSRequestCtx[traceEchoResponse](ctx, nc, "test.trace.echo", traceEchoRequest{Name: "spinifex"}, 5*time.Second, "123456789012")
	require.NoError(t, err)
	parent.End()

	assert.Equal(t, "spinifex", out.Name)
	assert.Equal(t, "123456789012", gotAccount, "AccountID header must coexist with traceparent")

	require.True(t, consumerSC.IsValid(), "consumer handler received no span context")
	assert.Equal(t, parent.SpanContext().TraceID(), consumerSC.TraceID(),
		"consumer span must join the producer's trace")

	// The consumer span's parent must be the producer's client span.
	var clientSpanID trace.SpanID
	var consumerParentID trace.SpanID
	for _, s := range sr.Ended() {
		switch s.SpanKind() {
		case trace.SpanKindClient:
			clientSpanID = s.SpanContext().SpanID()
		case trace.SpanKindConsumer:
			consumerParentID = s.Parent().SpanID()
		default:
		}
	}
	assert.Equal(t, clientSpanID, consumerParentID, "consumer span parent must be the producer client span")
}

// TestNATSRequestCtxNoSpanStillWorks: without a recording span the request
// must behave exactly like NATSRequest (no headers beyond AccountID required).
func TestNATSRequestCtxNoSpanStillWorks(t *testing.T) {
	ns := startTestNATSServer(t)
	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Close()

	_, err = nc.Subscribe("test.trace.plain", func(msg *nats.Msg) {
		ServeNATSRequest(msg, func(in *traceEchoRequest) (*traceEchoResponse, error) {
			return &traceEchoResponse{Name: in.Name}, nil
		})
	})
	require.NoError(t, err)

	out, err := NATSRequestCtx[traceEchoResponse](context.Background(), nc, "test.trace.plain", traceEchoRequest{Name: "plain"}, 5*time.Second, "123456789012")
	require.NoError(t, err)
	assert.Equal(t, "plain", out.Name)
}
