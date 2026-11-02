package pipeline

import (
	"github.com/go-chi/chi/v5"
	otelsetup "github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"slices"
	"testing"
)

func TestELBv2Pipeline_TraceActionEnricher_SpanNamedServiceAction(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))
	defer otel.SetTracerProvider(prev)

	gw, _ := elbv2PipelineApp(t, allowAllTaggingDocs(), nil)
	r := chi.NewRouter()
	r.Use(otelsetup.HTTPMiddleware("awsgw"))
	r.Use(gw.SigV4AuthMiddleware())
	r.Use(traceActionEnricher)
	r.HandleFunc("/*", gw.Request)

	listener := pipelineListener("web", "1", "2")
	doRequest(r, signELBv2Request(t, "Action=DescribeListenerAttributes&ListenerArn="+listener))

	spans := sr.Ended()
	idx := slices.IndexFunc(spans, func(s sdktrace.ReadOnlySpan) bool {
		return s.Name() == "elasticloadbalancing.DescribeListenerAttributes"
	})
	require.GreaterOrEqual(t, idx, 0, "no span named elasticloadbalancing.DescribeListenerAttributes among %d ended spans", len(spans))
	got := map[string]string{}
	for _, kv := range spans[idx].Attributes() {
		got[string(kv.Key)] = kv.Value.String()
	}
	assert.Equal(t, "DescribeListenerAttributes", got["aws.action"])
	assert.Equal(t, "elasticloadbalancing", got["aws.service"])
}
