package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/otelsetup"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestTraceActionEnricher(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))
	defer otel.SetTracerProvider(prev)

	// Simulate the auth middleware populating the SigV4 context, as
	// SigV4AuthMiddleware does, before the enricher runs.
	fakeAuth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), ctxAction, "RunInstances")
			ctx = context.WithValue(ctx, ctxService, "ec2")
			ctx = context.WithValue(ctx, ctxAccountID, "123456789012")
			ctx = context.WithValue(ctx, ctxRegion, "ap-southeast-2")
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h := otelsetup.HTTPMiddleware("awsgw")(fakeAuth(traceActionEnricher(final)))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil))

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	span := spans[0]
	if span.Name() != "ec2.RunInstances" {
		t.Errorf("span name = %q, want ec2.RunInstances", span.Name())
	}
	got := map[string]string{}
	for _, kv := range span.Attributes() {
		got[string(kv.Key)] = kv.Value.String()
	}
	for key, want := range map[string]string{
		"aws.action":     "RunInstances",
		"aws.service":    "ec2",
		"aws.account_id": "123456789012",
		"aws.region":     "ap-southeast-2",
	} {
		if got[key] != want {
			t.Errorf("attr %s = %q, want %q", key, got[key], want)
		}
	}
}
