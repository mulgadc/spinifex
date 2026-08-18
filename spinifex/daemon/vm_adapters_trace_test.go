package daemon

import (
	"encoding/json"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
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
	require.NoError(t, adapter.MountOne(t.Context(), "", req))

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
	require.NoError(t, adapter.UnmountOne(t.Context(), "", types.EBSRequest{Name: "vol-linked"}))

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

// mountOnceOn answers a single ebs.mount on subject and hands back the headers
// the adapter sent, so a test can assert on the wire rather than the span only.
func mountOnceOn(t *testing.T, nc *nats.Conn, subject string) <-chan nats.Header {
	t.Helper()
	headerCh := make(chan nats.Header, 1)
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		headerCh <- msg.Header
		data, marshalErr := json.Marshal(types.EBSMountResponse{URI: "nbd://vol"})
		require.NoError(t, marshalErr)
		require.NoError(t, msg.Respond(data))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return headerCh
}

// The ebs hop used to root its own trace from context.Background(), so block
// storage work appeared as an orphan trace no request could be followed into.
// Given a caller's context it must extend that trace instead.
func TestEbsRequestWithTrace_JoinsTheCallersTrace(t *testing.T) {
	sr := withRecordedSpans(t)
	daemon := createTestDaemon(t, sharedNATSURL)
	mountOnceOn(t, daemon.natsConn, "ebs.node-1.mount")

	adapter := newVolumeMounterAdapter(daemon.natsConn, daemon.node, nil)
	ctx, caller := otel.Tracer("test").Start(t.Context(), "caller")
	require.NoError(t, adapter.MountOne(ctx, "000000000042", &types.EBSRequest{Name: "vol-ctx"}))
	caller.End()

	var producer sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.SpanKind() == trace.SpanKindClient {
			producer = s
		}
	}
	require.NotNil(t, producer, "producer span must be recorded")

	assert.Equal(t, caller.SpanContext().TraceID(), producer.SpanContext().TraceID(),
		"the ebs span must join the caller's trace rather than root one of its own")
	assert.Equal(t, caller.SpanContext().SpanID(), producer.Parent().SpanID(),
		"the ebs span's parent must be the caller")
}

// One cluster serves many accounts and a volume belongs to exactly one of
// them. The account rides both the span and the header, because viperblockd
// reads the header to attribute its own consumer span.
func TestEbsRequestWithTrace_CarriesTheAccount(t *testing.T) {
	sr := withRecordedSpans(t)
	daemon := createTestDaemon(t, sharedNATSURL)
	headerCh := mountOnceOn(t, daemon.natsConn, "ebs.node-1.mount")

	adapter := newVolumeMounterAdapter(daemon.natsConn, daemon.node, nil)
	instance := &vm.VM{ID: "i-acct", AccountID: "000000000042"}
	instance.EBSRequests.Requests = []types.EBSRequest{{Name: "vol-acct"}}
	require.NoError(t, adapter.Mount(t.Context(), instance))

	assert.Equal(t, "000000000042", (<-headerCh).Get(utils.AccountIDHeader),
		"viperblockd reads the account off the header, so it must be set")

	var producer sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.SpanKind() == trace.SpanKindClient {
			producer = s
		}
	}
	require.NotNil(t, producer)
	assert.Equal(t, "000000000042", spanAccountOf(producer),
		"the ebs span must name the account it acted for")
}

// Recovery and teardown act for no caller. Crediting that work to an account
// would be worse than leaving it unattributed.
func TestEbsRequestWithTrace_OmitsAnAbsentAccount(t *testing.T) {
	sr := withRecordedSpans(t)
	daemon := createTestDaemon(t, sharedNATSURL)
	headerCh := mountOnceOn(t, daemon.natsConn, "ebs.node-1.mount")

	adapter := newVolumeMounterAdapter(daemon.natsConn, daemon.node, nil)
	require.NoError(t, adapter.MountOne(t.Context(), "", &types.EBSRequest{Name: "vol-none"}))

	assert.Empty(t, (<-headerCh).Get(utils.AccountIDHeader),
		"an unattributed request must not carry a blank account header")
	for _, s := range sr.Ended() {
		if s.SpanKind() == trace.SpanKindClient {
			assert.Empty(t, spanAccountOf(s), "an unattributed span must carry no account")
		}
	}
}

// spanAccountOf returns the account attribute of span, or "".
func spanAccountOf(span sdktrace.ReadOnlySpan) string {
	for _, attr := range span.Attributes() {
		if string(attr.Key) == utils.AttrAccountID {
			return attr.Value.AsString()
		}
	}
	return ""
}
