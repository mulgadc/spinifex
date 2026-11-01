package gateway

import (
	"github.com/go-chi/chi/v5"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	acmawsapi "github.com/mulgadc/spinifex/spinifex/domains/acm/awsapi"
	"github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

// TestACMActionFromTarget_FormsAndTelemetryAgree pins how acmActionFromTarget
// and the legacy resolveNonQueryAction each resolve the X-Amz-Target header
// today. A refactor that lets them diverge breaks this.
func TestACMActionFromTarget_FormsAndTelemetryAgree(t *testing.T) {
	cases := []struct {
		name       string
		target     string
		wantAction string
	}{
		{"service prefix", "CertificateManager.ListCertificates", "ListCertificates"},
		{"unrelated two-segment prefix", "a.b.ListCertificates", "ListCertificates"},
		{"bare action", "ListCertificates", "ListCertificates"},
		{"trailing dot", "CertificateManager.", ""},
	}
	acmEntry, ok := withACM(&GatewayConfig{}, acmawsapi.Deps{}).registered(acmawsapi.ServiceName)
	require.True(t, ok)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req.Header.Set("X-Amz-Target", tc.target)
			assert.Equal(t, tc.wantAction, acmEntry.ResolveAction(req))

			assert.Equal(t, tc.wantAction, resolveNonQueryAction(req),
				"dispatch and telemetry parsers must agree on %q", tc.target)
		})
	}
}

// TestACMPipeline_TraceActionEnricher_SpanNamedServiceAction pins that the
// server span is renamed to "acm.<Action>" and carries aws.action once a real
// ACM request runs through SigV4AuthMiddleware and traceActionEnricher.
func TestACMPipeline_TraceActionEnricher_SpanNamedServiceAction(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))
	defer otel.SetTracerProvider(prev)

	gw, _ := acmPipelineApp(t, allowAllACMDocs())
	subscribeACMCounter(t, gwfix.DepsOf(gw).NATS, "acm.ListCertificates", []byte(`{}`))

	r := chi.NewRouter()
	r.Use(otelsetup.HTTPMiddleware("awsgw"))
	r.Use(gw.SigV4AuthMiddleware())
	r.Use(traceActionEnricher)
	r.HandleFunc("/*", gw.Request)

	target := "CertificateManager.ListCertificates"
	doRequest(r, signACMRequest(t, target, []byte("{}")))

	// The HTTP server span is the one traceActionEnricher renames. A second,
	// unrelated client span covers the ListCertificates NATS hop itself.
	spans := sr.Ended()
	idx := slices.IndexFunc(spans, func(s sdktrace.ReadOnlySpan) bool { return s.Name() == "acm.ListCertificates" })
	require.GreaterOrEqual(t, idx, 0, "no span named acm.ListCertificates among %d ended spans", len(spans))
	span := spans[idx]
	got := map[string]string{}
	for _, kv := range span.Attributes() {
		got[string(kv.Key)] = kv.Value.String()
	}
	assert.Equal(t, "ListCertificates", got["aws.action"])
	assert.Equal(t, "acm", got["aws.service"])
}
