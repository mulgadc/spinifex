package daemon

import (
	"github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"testing"
	"time"

	viperblocklegacyv1 "github.com/mulgadc/spinifex/contracts/viperblockd/legacy/v1"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
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

// requestOnceOn answers a single request on subject with an empty JSON body
// and hands back the headers it carried, so a test can assert on the wire.
func requestOnceOn(t *testing.T, nc *nats.Conn, subject string) <-chan nats.Header {
	t.Helper()
	headerCh := make(chan nats.Header, 1)
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		headerCh <- msg.Header
		require.NoError(t, msg.Respond([]byte("{}")))
	})
	require.NoError(t, err)
	require.NoError(t, nc.Flush())
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return headerCh
}

// TestEbsRequestWithTrace_HeaderCarriesTraceparent pins the minimum bar: the
// outbound ebs.* NATS message carries a non-empty traceparent header, so a
// consumer that calls natsmsg.ExtractTraceContext has something to join.
func TestEbsRequestWithTrace_HeaderCarriesTraceparent(t *testing.T) {
	withRecordedSpans(t)
	nc := connectMounterTest(t)
	subject := "ebs." + newMounterTestNode() + ".delete"
	headerCh := requestOnceOn(t, nc, subject)

	_, err := ebsRequestWithTrace(t.Context(), nc, "", subject, []byte("{}"), 5*time.Second)
	require.NoError(t, err)

	assert.NotEmpty(t, (<-headerCh).Get("traceparent"), "outbound ebs.* request must carry a traceparent header")
}

// TestEbsRequestWithTrace_ProducerConsumerLinked proves the actual parent/child
// linkage: the consumer span opened via natsmsg.StartConsumerSpan (the helper
// viperblockd's legacy handlers use) shares the producer span's trace ID and
// names it as its parent, rather than rooting a new trace.
func TestEbsRequestWithTrace_ProducerConsumerLinked(t *testing.T) {
	sr := withRecordedSpans(t)
	nc := connectMounterTest(t)
	subject := "ebs." + newMounterTestNode() + ".delete"

	consumerEnded := make(chan struct{})
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		defer close(consumerEnded)
		_, span := natsmsg.StartConsumerSpan(msg)
		defer span.End()
		require.NoError(t, msg.Respond([]byte("{}")))
	})
	require.NoError(t, err)
	require.NoError(t, nc.Flush())
	defer sub.Unsubscribe()

	_, err = ebsRequestWithTrace(t.Context(), nc, "", subject, []byte("{}"), 5*time.Second)
	require.NoError(t, err)
	// The handler replies before its deferred span.End runs, so the consumer
	// span may not be recorded yet when the request returns.
	<-consumerEnded

	spans := sr.Ended()
	require.Len(t, spans, 2, "expected one producer span and one consumer span")

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

// The ebs hop used to root its own trace from context.Background(), so block
// storage work appeared as an orphan trace no request could be followed into.
// Given a caller's context it must extend that trace instead.
func TestEbsRequestWithTrace_JoinsTheCallersTrace(t *testing.T) {
	sr := withRecordedSpans(t)
	nc := connectMounterTest(t)
	subject := "ebs." + newMounterTestNode() + ".delete"
	requestOnceOn(t, nc, subject)

	ctx, caller := otel.Tracer("test").Start(t.Context(), "caller")
	_, err := ebsRequestWithTrace(ctx, nc, "000000000042", subject, []byte("{}"), 5*time.Second)
	require.NoError(t, err)
	caller.End()

	producer := clientSpan(t, sr)
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
	nc := connectMounterTest(t)
	subject := "ebs." + newMounterTestNode() + ".delete"
	headerCh := requestOnceOn(t, nc, subject)

	_, err := ebsRequestWithTrace(t.Context(), nc, "000000000042", subject, []byte("{}"), 5*time.Second)
	require.NoError(t, err)

	assert.Equal(t, "000000000042", (<-headerCh).Get(natsmsg.AccountIDHeader),
		"viperblockd reads the account off the header, so it must be set")
	assert.Equal(t, "000000000042", spanAccountOf(clientSpan(t, sr)),
		"the ebs span must name the account it acted for")
}

// Recovery and teardown act for no caller. Crediting that work to an account
// would be worse than leaving it unattributed.
func TestEbsRequestWithTrace_OmitsAnAbsentAccount(t *testing.T) {
	sr := withRecordedSpans(t)
	nc := connectMounterTest(t)
	subject := "ebs." + newMounterTestNode() + ".delete"
	headerCh := requestOnceOn(t, nc, subject)

	_, err := ebsRequestWithTrace(t.Context(), nc, "", subject, []byte("{}"), 5*time.Second)
	require.NoError(t, err)

	assert.Empty(t, (<-headerCh).Get(natsmsg.AccountIDHeader),
		"an unattributed request must not carry a blank account header")
	assert.Empty(t, spanAccountOf(clientSpan(t, sr)), "an unattributed span must carry no account")
}

// A mount names its account on the mounter's span, which the provider's
// request span joins under the caller's trace, and an unattributed one names
// none.
func TestVolumeMounterAdapter_ContractSpansCarryTheAccountUnderTheCaller(t *testing.T) {
	sr := withRecordedSpans(t)
	nc := connectMounterTest(t)
	node := newMounterTestNode()
	serveFakeNodeProvider(t, nc, node)
	adapter := newVolumeMounterAdapter(nc, node, nil)

	ctx, caller := otel.Tracer("test").Start(t.Context(), "caller")
	instance := &vm.VM{ID: "i-acct", AccountID: "000000000042"}
	instance.EBSRequests.Requests = []viperblocklegacyv1.EBSRequest{{Name: "vol-acct"}}
	require.NoError(t, adapter.Mount(ctx, instance))
	require.NoError(t, adapter.MountOne(ctx, "", &viperblocklegacyv1.EBSRequest{Name: "vol-none"}))
	caller.End()

	spans := map[string]sdktrace.ReadOnlySpan{}
	var request sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == "ebs.mounter.publish" {
			spans[volumeOf(s)] = s
		} else if s.SpanKind() == trace.SpanKindClient && request == nil {
			request = s
		}
	}
	mount, unattributed := spans["vol-acct"], spans["vol-none"]
	require.NotNil(t, mount)
	require.NotNil(t, unattributed)
	require.NotNil(t, request, "the provider request must record a client span")

	assert.Equal(t, "000000000042", spanAccountOf(mount), "the mounter span must name the account it acted for")
	assert.Empty(t, spanAccountOf(unattributed), "an unattributed mount must carry no account")
	assert.Equal(t, caller.SpanContext().SpanID(), mount.Parent().SpanID(), "the mounter span must join the caller")
	assert.Equal(t, mount.SpanContext().SpanID(), request.Parent().SpanID(), "the provider request must sit under the mounter span")
}

// clientSpan returns the one client span sr recorded.
func clientSpan(t *testing.T, sr *tracetest.SpanRecorder) sdktrace.ReadOnlySpan {
	t.Helper()
	var spans []sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.SpanKind() == trace.SpanKindClient {
			spans = append(spans, s)
		}
	}
	require.Len(t, spans, 1)
	return spans[0]
}

// spanAccountOf returns the account attribute of span, or "".
func spanAccountOf(span sdktrace.ReadOnlySpan) string {
	return spanAttribute(span, natsmsg.AttrAccountID)
}

func volumeOf(span sdktrace.ReadOnlySpan) string {
	return spanAttribute(span, "volume.id")
}

func spanAttribute(span sdktrace.ReadOnlySpan, key string) string {
	for _, attr := range span.Attributes() {
		if string(attr.Key) == key {
			return attr.Value.AsString()
		}
	}
	return ""
}
