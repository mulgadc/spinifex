package daemon

import (
	"encoding/json"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// withRecordedSpans installs a fresh SpanRecorder as the global tracer
// provider for the duration of the test and restores the previous provider
// on cleanup. It also installs the W3C TraceContext propagator that
// otelsetup.Init installs in production — the global default is a no-op that
// injects nothing, which would silently defeat InjectTraceContext here.
func withRecordedSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	prevProvider := otel.GetTracerProvider()
	prevPropagator := otel.GetTextMapPropagator()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))
	t.Cleanup(func() {
		otel.SetTracerProvider(prevProvider)
		otel.SetTextMapPropagator(prevPropagator)
	})
	return sr
}

// TestEbsRequestWithTrace_HeaderCarriesTraceparent pins the minimum bar: the
// outbound ebs.mount NATS message carries a non-empty traceparent header, so
// a consumer that calls utils.ExtractTraceContext has something to join.
func TestEbsRequestWithTrace_HeaderCarriesTraceparent(t *testing.T) {
	withRecordedSpans(t)
	daemon := createTestDaemon(t, sharedNATSURL)

	headerCh := make(chan nats.Header, 1)
	sub, err := daemon.natsConn.Subscribe("ebs.node-1.mount", func(msg *nats.Msg) {
		headerCh <- msg.Header
		resp := types.EBSMountResponse{URI: "nbd://traced-vol"}
		data, marshalErr := json.Marshal(resp)
		require.NoError(t, marshalErr)
		require.NoError(t, msg.Respond(data))
	})
	require.NoError(t, err)
	defer sub.Unsubscribe()

	adapter := newVolumeMounterAdapter(daemon.natsConn, daemon.node, nil)
	req := &types.EBSRequest{Name: "vol-traced", DeviceName: "/dev/sdf"}
	require.NoError(t, adapter.MountOne(req))

	hdr := <-headerCh
	assert.NotEmpty(t, hdr.Get("traceparent"), "outbound ebs.mount request must carry a traceparent header")
}

// TestEbsRequestWithTrace_ProducerConsumerLinked proves the actual parent/child
// linkage: the consumer span opened via utils.StartConsumerSpan (the same
// helper viperblockd's ebs.mount/ebs.unmount handlers use) shares the producer
// span's trace ID and names it as its parent, rather than rooting a new trace
// — the exact gap this fix closes for the 81 mount + 81 unmount rooted spans
// the trace analysis found.
func TestEbsRequestWithTrace_ProducerConsumerLinked(t *testing.T) {
	sr := withRecordedSpans(t)
	daemon := createTestDaemon(t, sharedNATSURL)

	sub, err := daemon.natsConn.Subscribe("ebs.node-1.unmount", func(msg *nats.Msg) {
		// Mirrors viperblockd's ebs.unmount handler: open a consumer span
		// joining whatever trace context the producer injected.
		_, span := utils.StartConsumerSpan(msg)
		defer span.End()

		resp := types.EBSUnMountResponse{}
		data, marshalErr := json.Marshal(resp)
		require.NoError(t, marshalErr)
		require.NoError(t, msg.Respond(data))
	})
	require.NoError(t, err)
	defer sub.Unsubscribe()

	adapter := newVolumeMounterAdapter(daemon.natsConn, daemon.node, nil)
	require.NoError(t, adapter.UnmountOne(types.EBSRequest{Name: "vol-linked"}))

	spans := sr.Ended()
	require.Len(t, spans, 2, "expected one producer span and one consumer span")

	// Both spans share the "NATS ebs.node-1.unmount" name; distinguish by kind
	// (producer = client, consumer = server, as set by ebsRequestWithTrace and
	// utils.StartConsumerSpan respectively).
	var producer, consumer sdktrace.ReadOnlySpan
	for _, s := range spans {
		switch s.SpanKind() {
		case trace.SpanKindClient:
			producer = s
		case trace.SpanKindConsumer:
			consumer = s
		}
	}
	require.NotNil(t, producer, "producer span must be recorded")
	require.NotNil(t, consumer, "consumer span must be recorded")

	assert.Equal(t, producer.SpanContext().TraceID(), consumer.SpanContext().TraceID(),
		"consumer span must join the producer's trace, not root a new one")
	assert.Equal(t, producer.SpanContext().SpanID(), consumer.Parent().SpanID(),
		"consumer span's parent must be the producer span")
}
