// Package otelsetup configures OpenTelemetry tracing and metrics for Mulga
// services. Export is gated on the standard OTLP environment variables; with
// no endpoint configured Init is a functional no-op, so instrumented binaries
// ship everywhere at zero cost.
package otelsetup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	otelruntime "go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	loggerglobal "go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

const shutdownTimeout = 10 * time.Second

// Init installs global tracer and meter providers exporting OTLP over gRPC,
// plus the W3C trace-context propagator. The returned shutdown func flushes
// and stops both providers; it is always safe to call. When no OTLP endpoint
// is configured (or OTEL_SDK_DISABLED=true) only the propagator is installed
// and the globals stay no-op.
func Init(ctx context.Context, serviceName string) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))

	if !exportEnabled() {
		return func(context.Context) error { return nil }, nil
	}

	res, err := newResource(ctx, serviceName)
	if err != nil {
		return nil, fmt.Errorf("otel resource for %s: %w", serviceName, err)
	}

	traceExp, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("otlp trace exporter for %s: %w", serviceName, err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(rootSampler()),
	)
	otel.SetTracerProvider(tp)

	metricExp, err := otlpmetricgrpc.New(ctx)
	if err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		return nil, errors.Join(
			fmt.Errorf("otlp metric exporter for %s: %w", serviceName, err),
			tp.Shutdown(shutdownCtx))
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mp)

	logExp, err := otlploggrpc.New(ctx)
	if err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		return nil, errors.Join(
			fmt.Errorf("otlp log exporter for %s: %w", serviceName, err),
			tp.Shutdown(shutdownCtx), mp.Shutdown(shutdownCtx))
	}
	lp := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp)),
	)
	loggerglobal.SetLoggerProvider(lp)
	addFanoutHandler(otelslog.NewHandler(serviceName, otelslog.WithLoggerProvider(lp)))

	if err := otelruntime.Start(); err != nil {
		slog.Warn("otel runtime metrics disabled", "err", err)
	}

	shutdown := func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, shutdownTimeout)
		defer cancel()
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx), lp.Shutdown(ctx))
	}
	return shutdown, nil
}

// exportEnabled reports whether any standard OTLP endpoint is configured and
// the SDK is not explicitly disabled.
// rootSampler samples locally-rooted traces at MULGA_ROOT_TRACE_RATIO
// (default 1.0) while always honoring an inbound sampled traceparent. Lets
// chatty services shed root noise without losing any request-linked span.
func rootSampler() sdktrace.Sampler {
	ratio := 1.0
	if v := os.Getenv("MULGA_ROOT_TRACE_RATIO"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 && f <= 1 {
			ratio = f
		} else {
			slog.Warn("invalid MULGA_ROOT_TRACE_RATIO, using 1.0", "value", v)
		}
	}
	return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))
}

func exportEnabled() bool {
	if strings.EqualFold(os.Getenv("OTEL_SDK_DISABLED"), "true") {
		return false
	}
	for _, key := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
	} {
		if os.Getenv(key) != "" {
			return true
		}
	}
	return false
}

// newResource builds the service resource: identity attrs set here, plus
// host detection and anything in OTEL_RESOURCE_ATTRIBUTES (ci.run_id etc.).
func newResource(ctx context.Context, serviceName string) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{semconv.ServiceName(serviceName)}
	if v := buildVersion(); v != "" {
		attrs = append(attrs, semconv.ServiceVersion(v))
	}
	env := os.Getenv("MULGA_ENV")
	if env == "" {
		env = os.Getenv("SPINIFEX_CI_ENV")
	}
	if env != "" {
		// deployment.environment maps to service.environment in Elastic APM,
		// enabling the native environment selector across the APM UI.
		attrs = append(attrs,
			attribute.String("mulga.env", env),
			semconv.DeploymentEnvironment(env))
	}
	if src := os.Getenv("MULGA_SOURCE"); src != "" {
		attrs = append(attrs, attribute.String("mulga.source", src))
	}

	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithHost(),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(attrs...),
	)
	// Schema-URL conflicts between detectors still yield a usable merged
	// resource; only a nil resource is fatal.
	if err != nil && res == nil {
		return nil, err
	}
	return res, nil
}

// buildVersion returns the module version or embedded VCS revision, if any.
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && len(s.Value) >= 12 {
			return s.Value[:12]
		}
	}
	return ""
}
