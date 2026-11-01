package pipeline

import (
	"github.com/go-chi/chi/v5"
	"github.com/mulgadc/spinifex/spinifex/domains/tagging"
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

func TestTaggingActionFromTarget_FormsAndTelemetryAgree(t *testing.T) {
	cases := []struct {
		name, target, wantAction string
	}{
		{"service prefix", taggingTargetPrefix + "GetResources", "GetResources"},
		{"unrelated two-segment prefix", "a.b.GetResources", "GetResources"},
		{"bare action", "GetResources", "GetResources"},
		{"trailing dot", taggingTargetPrefix, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req.Header.Set("X-Amz-Target", tc.target)
			gw := withTagging(&Config{}, tagging.NewAggregator())
			assert.Equal(t, tc.wantAction, gw.selector().ResolveAction(req, "tagging"))
			assert.Equal(t, tc.wantAction, resolveNonQueryAction(req))
		})
	}
}

func TestTaggingPipeline_TraceActionEnricher_SpanNamedServiceAction(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))
	defer otel.SetTracerProvider(prev)

	gw, _ := taggingPipelineApp(t, allowAllTaggingDocs(), tagging.NewAggregator())
	r := chi.NewRouter()
	r.Use(otelsetup.HTTPMiddleware("awsgw"))
	r.Use(gw.SigV4AuthMiddleware())
	r.Use(traceActionEnricher)
	r.HandleFunc("/*", gw.Request)

	doRequest(r, signTaggingRequest(t, taggingTargetPrefix+"GetResources", []byte("{}")))

	spans := sr.Ended()
	idx := slices.IndexFunc(spans, func(s sdktrace.ReadOnlySpan) bool { return s.Name() == "tagging.GetResources" })
	require.GreaterOrEqual(t, idx, 0, "no span named tagging.GetResources among %d ended spans", len(spans))
	got := map[string]string{}
	for _, kv := range spans[idx].Attributes() {
		got[string(kv.Key)] = kv.Value.String()
	}
	assert.Equal(t, "GetResources", got["aws.action"])
	assert.Equal(t, "tagging", got["aws.service"])
}
