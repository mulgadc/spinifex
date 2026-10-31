package gateway

import (
	"github.com/mulgadc/bluebottle/pkg/ratelimit"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	otelsetup "github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/audit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"net/http"
	"slices"
	"testing"
)

// The cluster-unavailable gate runs ahead of everything after authentication:
// a caller the policy would refuse, an action that does not exist and a
// gateway-local action all get the same 503, so none of them reach dispatch.
func TestEC2Labels_ClusterUnavailableGateOrder(t *testing.T) {
	cases := []struct {
		name string
		deny bool
		body string
	}{
		{"policy would deny", true, "Action=DescribeVpcs"},
		{"unknown action", false, "Action=NoSuchAction"},
		{"missing action", false, "Foo=bar"},
		{"gateway-local DescribeRegions", false, "Action=DescribeRegions"},
		{"dry run", false, "Action=DescribeVpcs&DryRun=true"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stmts []iamdomain.Statement
			if !c.deny {
				stmts = append(stmts, allowEC2())
			}
			app := ec2Pipeline(t, func(e *ec2PipelineEnv) { e.NoNATS = true }, stmts...)
			got := probeEC2(t, app.Handler, c.body)
			assert.Equal(t, http.StatusServiceUnavailable, got.Status, got.Body)
			assert.Equal(t, "ServiceUnavailable", got.Code)
			assert.Contains(t, got.Message, "cluster unavailable", got.Message)
		})
	}
}

// The policy gate evaluates service "ec2" and the request's own Action: a
// caller allowed only ec2:<Action> is not refused, so the gate names no other
// action and no other service.
func TestEC2Labels_PolicyGateEvaluatesServiceAndAction(t *testing.T) {
	for _, c := range ec2LabelCases {
		t.Run(c.action, func(t *testing.T) {
			app := ec2Pipeline(t, nil, statement("Allow", "ec2:"+c.action, "*"))
			got := probeEC2(t, app.Handler, c.body)
			assert.NotEqual(t, "UnauthorizedOperation", got.Code, got.Body)

			other := ec2Pipeline(t, nil, statement("Allow", "ec2:NotThisAction", "*"))
			denied := probeEC2(t, other.Handler, c.body)
			assert.Equal(t, "UnauthorizedOperation", denied.Code, denied.Body)
		})
	}
}

// The server span is renamed ec2.<Action> and tagged with the service and
// action, and the audit record carries the same action.
func TestEC2Labels_SpanAndAudit(t *testing.T) {
	for _, c := range ec2LabelCases {
		t.Run(c.action, func(t *testing.T) {
			sr := tracetest.NewSpanRecorder()
			prev := otel.GetTracerProvider()
			otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))
			defer otel.SetTracerProvider(prev)

			var rec *audit.Record
			capture := func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					next.ServeHTTP(w, r)
					rec = audit.From(r.Context())
				})
			}
			app := ec2Pipeline(t, func(e *ec2PipelineEnv) {
				e.Outer = []func(http.Handler) http.Handler{otelsetup.HTTPMiddleware("awsgw"), audit.Middleware, capture}
			}, allowEC2())
			probeEC2(t, app.Handler, c.body)

			spans := sr.Ended()
			idx := slices.IndexFunc(spans, func(s sdktrace.ReadOnlySpan) bool { return s.Name() == "ec2."+c.action })
			require.GreaterOrEqual(t, idx, 0, "no span named ec2.%s among %d ended spans", c.action, len(spans))
			attrs := map[string]string{}
			for _, kv := range spans[idx].Attributes() {
				attrs[string(kv.Key)] = kv.Value.String()
			}
			assert.Equal(t, c.action, attrs["aws.action"])
			assert.Equal(t, "ec2", attrs["aws.service"])

			require.NotNil(t, rec)
			assert.Equal(t, c.action, auditField(rec, "action"))
		})
	}
}

// The throttle bucket is per account and action: a second call to one action is
// refused while a different action still passes, and the refusal is EC2's
// RequestLimitExceeded in the XML envelope.
func TestEC2Labels_ThrottleKeyIsAccountAndAction(t *testing.T) {
	throttler := ratelimit.New(ratelimit.Config{Enabled: true, Rate: 1, Burst: 1})
	defer throttler.Stop()
	app := ec2Pipeline(t, func(e *ec2PipelineEnv) { e.Throttler = throttler }, allowEC2())

	first := probeEC2(t, app.Handler, "Action=DescribeVpcs")
	assert.Less(t, first.Status, 400, first.Body)

	second := probeEC2(t, app.Handler, "Action=DescribeVpcs")
	assert.Equal(t, http.StatusServiceUnavailable, second.Status, second.Body)
	assert.Equal(t, "RequestLimitExceeded", second.Code)
	assert.Contains(t, second.ContentType, "xml")

	other := probeEC2(t, app.Handler, "Action=DescribeInstances")
	assert.Less(t, other.Status, 400, other.Body)
}
