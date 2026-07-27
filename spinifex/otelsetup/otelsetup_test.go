package otelsetup

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	loggerglobal "go.opentelemetry.io/otel/log/global"
	lognoop "go.opentelemetry.io/otel/log/noop"
)

func TestInitWithoutEndpointIsNoop(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")
	// Pin a known no-op provider: the otel global delegator cannot be reset
	// once another test has installed a real provider.
	otel.SetTracerProvider(noop.NewTracerProvider())

	shutdown, err := Init(context.Background(), "test-svc")
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	_, span := otel.Tracer("test").Start(context.Background(), "op")
	defer span.End()
	if span.SpanContext().IsValid() {
		t.Error("expected no-op tracer without endpoint, got recording span")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown: %v", err)
	}
}

func TestInitWithEndpointInstallsProviders(t *testing.T) {
	// Point at a dead endpoint: exporters dial lazily, so Init must still
	// succeed and install real (recording) providers.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	prevTracer := otel.GetTracerProvider()
	prevMeter := otel.GetMeterProvider()
	defer otel.SetTracerProvider(prevTracer)
	defer otel.SetMeterProvider(prevMeter)

	shutdown, err := Init(context.Background(), "test-svc")
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	_, span := otel.Tracer("test").Start(context.Background(), "op")
	if !span.SpanContext().IsValid() {
		t.Error("expected recording tracer with endpoint set, got no-op span")
	}
	span.End()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	// Flush hits the dead endpoint; only assert it returns rather than hangs.
	_ = shutdown(ctx)
}

func TestExportEnabled(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"nothing set", nil, false},
		{"endpoint set", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://localhost:4317"}, true},
		{"traces endpoint only", map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://localhost:4317"}, true},
		{"disabled overrides endpoint", map[string]string{
			"OTEL_EXPORTER_OTLP_ENDPOINT": "http://localhost:4317",
			"OTEL_SDK_DISABLED":           "true",
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, key := range []string{
				"OTEL_EXPORTER_OTLP_ENDPOINT",
				"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
				"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
				"OTEL_SDK_DISABLED",
			} {
				t.Setenv(key, tt.env[key])
			}
			if got := exportEnabled(); got != tt.want {
				t.Errorf("exportEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNewResourceAttributes(t *testing.T) {
	t.Setenv("MULGA_ENV", "env19")
	t.Setenv("MULGA_SOURCE", "ci")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "ci.run_id=12345")

	res, err := newResource(context.Background(), "test-svc")
	if err != nil {
		t.Fatalf("newResource: %v", err)
	}
	got := map[string]string{}
	for _, kv := range res.Attributes() {
		got[string(kv.Key)] = kv.Value.String()
	}
	for key, want := range map[string]string{
		"service.name": "test-svc",
		"mulga.env":    "env19",
		"mulga.source": "ci",
		"ci.run_id":    "12345",
	} {
		if got[key] != want {
			t.Errorf("resource attr %s = %q, want %q", key, got[key], want)
		}
	}
	if got["host.name"] == "" {
		t.Error("resource attr host.name missing")
	}
}

func TestSlogHandlerStampsTraceIDs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(NewSlogHandler(slog.NewJSONHandler(&buf, nil)))

	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10},
		SpanID:  trace.SpanID{0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x01, 0x02},
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	logger.InfoContext(ctx, "with span")

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("unmarshal log line: %v", err)
	}
	if line["trace_id"] != sc.TraceID().String() {
		t.Errorf("trace_id = %v, want %s", line["trace_id"], sc.TraceID())
	}
	if line["span_id"] != sc.SpanID().String() {
		t.Errorf("span_id = %v, want %s", line["span_id"], sc.SpanID())
	}
}

func TestSlogHandlerNoSpanNoStamp(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(NewSlogHandler(slog.NewJSONHandler(&buf, nil)))

	logger.InfoContext(context.Background(), "no span")

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("unmarshal log line: %v", err)
	}
	if _, ok := line["trace_id"]; ok {
		t.Error("trace_id present on record without span")
	}
	if _, ok := line["span_id"]; ok {
		t.Error("span_id present on record without span")
	}
}

// TestInitWithoutEndpointInstallsNoLoggerProvider is the prod-safety
// guarantee: with no OTLP endpoint configured, Init must not install a
// LoggerProvider (the global stays whatever was there before) and must not
// touch the process default slog handler.
func TestInitWithoutEndpointInstallsNoLoggerProvider(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")

	prevLP := loggerglobal.GetLoggerProvider()
	defer loggerglobal.SetLoggerProvider(prevLP)
	sentinelLP := lognoop.NewLoggerProvider()
	loggerglobal.SetLoggerProvider(sentinelLP)

	prevHandler := slog.Default().Handler()
	defer slog.SetDefault(slog.New(prevHandler))
	sentinelHandler := slog.NewJSONHandler(&bytes.Buffer{}, nil)
	slog.SetDefault(slog.New(sentinelHandler))

	shutdown, err := Init(context.Background(), "test-svc")
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	if _, ok := loggerglobal.GetLoggerProvider().(lognoop.LoggerProvider); !ok {
		t.Errorf("expected global LoggerProvider to remain the noop sentinel, got %T", loggerglobal.GetLoggerProvider())
	}
	if got := slog.Default().Handler(); got != slog.Handler(sentinelHandler) {
		t.Errorf("expected default slog handler unchanged, got %T", got)
	}
}

// TestInitWithEndpointInstallsLoggerProvider proves the same gate installs a
// real LoggerProvider and fans the default logger out once an OTLP endpoint
// is configured.
func TestInitWithEndpointInstallsLoggerProvider(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	prevLP := loggerglobal.GetLoggerProvider()
	defer loggerglobal.SetLoggerProvider(prevLP)
	prevTracer := otel.GetTracerProvider()
	prevMeter := otel.GetMeterProvider()
	defer otel.SetTracerProvider(prevTracer)
	defer otel.SetMeterProvider(prevMeter)
	prevHandler := slog.Default().Handler()
	defer slog.SetDefault(slog.New(prevHandler))

	shutdown, err := Init(context.Background(), "test-svc")
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	if _, ok := loggerglobal.GetLoggerProvider().(*sdklog.LoggerProvider); !ok {
		t.Errorf("expected a real sdk/log LoggerProvider, got %T", loggerglobal.GetLoggerProvider())
	}
	if _, ok := slog.Default().Handler().(*fanoutHandler); !ok {
		t.Errorf("expected default slog handler rewrapped as fanoutHandler, got %T", slog.Default().Handler())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = shutdown(ctx)
}

// recordingLogExporter is a minimal sdk/log.Exporter that records every
// exported record for assertions, used since v0.20.0 ships no built-in
// in-memory test exporter for external consumers.
type recordingLogExporter struct {
	mu      sync.Mutex
	records []sdklog.Record
}

var _ sdklog.Exporter = (*recordingLogExporter)(nil)

func (e *recordingLogExporter) Export(_ context.Context, records []sdklog.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range records {
		e.records = append(e.records, r.Clone())
	}
	return nil
}

func (e *recordingLogExporter) Shutdown(context.Context) error   { return nil }
func (e *recordingLogExporter) ForceFlush(context.Context) error { return nil }

func (e *recordingLogExporter) snapshot() []sdklog.Record {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]sdklog.Record(nil), e.records...)
}

// TestLogRecordCarriesResourceAndTraceID exercises the exact pipeline Init
// wires (resource -> LoggerProvider -> otelslog bridge) with a recording
// exporter standing in for the OTLP gRPC exporter, proving exported records
// carry the full resource (incl. ci.run_id from OTEL_RESOURCE_ATTRIBUTES) and
// trace_id/span_id from the logging context.
func TestLogRecordCarriesResourceAndTraceID(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "ci.run_id=TESTRUN")

	res, err := newResource(context.Background(), "test-svc")
	if err != nil {
		t.Fatalf("newResource: %v", err)
	}

	exp := &recordingLogExporter{}
	lp := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewSimpleProcessor(exp)),
	)
	defer func() { _ = lp.Shutdown(context.Background()) }()

	bridge := otelslog.NewHandler("test-svc", otelslog.WithLoggerProvider(lp))
	logger := slog.New(bridge)

	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10},
		SpanID:     trace.SpanID{0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x01, 0x02},
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	logger.InfoContext(ctx, "hello from bridge")

	records := exp.snapshot()
	if len(records) != 1 {
		t.Fatalf("got %d exported records, want 1", len(records))
	}
	rec := records[0]

	if rec.TraceID() != sc.TraceID() {
		t.Errorf("record TraceID = %s, want %s", rec.TraceID(), sc.TraceID())
	}
	if rec.SpanID() != sc.SpanID() {
		t.Errorf("record SpanID = %s, want %s", rec.SpanID(), sc.SpanID())
	}

	attrs := map[string]string{}
	for _, kv := range rec.Resource().Attributes() {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	if attrs["ci.run_id"] != "TESTRUN" {
		t.Errorf("resource ci.run_id = %q, want TESTRUN", attrs["ci.run_id"])
	}
	if attrs["service.name"] != "test-svc" {
		t.Errorf("resource service.name = %q, want test-svc", attrs["service.name"])
	}
}

// recordingSlogHandler is a bare slog.Handler that records whether Handle
// was called, standing in for the pre-existing stdout/journald handler in
// fan-out tests.
type recordingSlogHandler struct {
	mu     sync.Mutex
	called int
}

var _ slog.Handler = (*recordingSlogHandler)(nil)

func (h *recordingSlogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingSlogHandler) Handle(context.Context, slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.called++
	return nil
}

func (h *recordingSlogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingSlogHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingSlogHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.called
}

// TestFanoutHandlerWritesToAllHandlers proves the existing stdout/journald
// handler still fires when the OTLP bridge handler is fanned in alongside it.
func TestFanoutHandlerWritesToAllHandlers(t *testing.T) {
	stdout := &recordingSlogHandler{}
	bridge := &recordingSlogHandler{}
	logger := slog.New(newFanoutHandler(stdout, bridge))

	logger.Info("dual write")

	if stdout.count() != 1 {
		t.Errorf("stdout handler called %d times, want 1", stdout.count())
	}
	if bridge.count() != 1 {
		t.Errorf("bridge handler called %d times, want 1", bridge.count())
	}
}

// TestAddFanoutHandlerPreservesExistingDefault proves addFanoutHandler
// rewraps rather than replaces whatever default handler was already
// installed (e.g. by SetDefaultJSONLogger before Init runs).
func TestAddFanoutHandlerPreservesExistingDefault(t *testing.T) {
	prevHandler := slog.Default().Handler()
	defer slog.SetDefault(slog.New(prevHandler))

	existing := &recordingSlogHandler{}
	slog.SetDefault(slog.New(existing))

	bridge := &recordingSlogHandler{}
	addFanoutHandler(bridge)

	slog.Info("after rewrap")

	if existing.count() != 1 {
		t.Errorf("pre-existing handler called %d times, want 1", existing.count())
	}
	if bridge.count() != 1 {
		t.Errorf("bridge handler called %d times, want 1", bridge.count())
	}
}

func TestSlogHandlerPreservesWrapperThroughWithAttrs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(NewSlogHandler(slog.NewJSONHandler(&buf, nil))).
		With("component", "test").WithGroup("grp")

	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{0xff, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10},
		SpanID:  trace.SpanID{0xfa, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x01, 0x02},
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	logger.InfoContext(ctx, "wrapped", "k", "v")

	if !bytes.Contains(buf.Bytes(), []byte(sc.TraceID().String())) {
		t.Errorf("trace_id lost after With/WithGroup: %s", buf.String())
	}
}
