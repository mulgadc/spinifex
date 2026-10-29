// Characterization tests for ELBv2 Query-protocol dispatch as it exists in
// elbv2.go today, pinned ahead of moving dispatch out of gateway into a
// registration seam. These assert current behaviour, not AWS parity.

package gateway

import (
	"bytes"
	"encoding/xml"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/credverify"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/mulgadc/bluebottle/pkg/sigv4"
	"github.com/mulgadc/spinifex/internal/testkit"
	admissionquota "github.com/mulgadc/spinifex/spinifex/domains/admission/quota"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	otelsetup "github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/audit"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

var requestIDPattern = regexp.MustCompile(`<RequestId>[^<]*</RequestId>`)

// elbv2PipelineApp wires SigV4AuthMiddleware -> Request as SetupRoutes does, on
// live test NATS, with docs as every principal's policy.
func elbv2PipelineApp(t *testing.T, docs []iamdomain.PolicyDocument, quota *admissionquota.Service) (*GatewayConfig, http.Handler) {
	t.Helper()
	encryptedSecret, err := iamdomain.EncryptSecret(testSecretKey, testMasterKey)
	require.NoError(t, err)
	_, nc := testutil.StartTestNATS(t)

	gw := &GatewayConfig{
		DisableLogging: true,
		Region:         testRegion,
		NATSConn:       nc,
		Quota:          quota,
		IAMService: &policyMockIAMService{
			mockIAMService: mockIAMService{
				masterKey: testMasterKey,
				accessKeys: map[string]*iamdomain.AccessKey{
					testAccessKey: {
						AccessKeyID:     testAccessKey,
						SecretAccessKey: encryptedSecret,
						UserName:        "alice",
						AccountID:       authzAccountID,
						Status:          "Active",
					},
				},
			},
			getUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) { return docs, nil },
		},
		Credentials: credverify.New(&policyMockIAMService{
			mockIAMService: mockIAMService{
				masterKey: testMasterKey,
				accessKeys: map[string]*iamdomain.AccessKey{
					testAccessKey: {
						AccessKeyID:     testAccessKey,
						SecretAccessKey: encryptedSecret,
						UserName:        "alice",
						AccountID:       authzAccountID,
						Status:          "Active",
					},
				},
			},
			getUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) { return docs, nil },
		}, nil),
	}
	withELBv2(gw)

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", gw.Request)
	return gw, r
}

// signELBv2Request builds a signed Query-protocol POST for the
// elasticloadbalancing credential scope.
func signELBv2Request(t *testing.T, body string) *http.Request {
	t.Helper()
	b := []byte(body)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(b))
	req.Host = "localhost:9999"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	signTestRequestAs(t, req, b, testAccessKey, testSecretKey, "elasticloadbalancing", time.Now().UTC())
	return req
}

// respondELBv2 answers subject with payload and counts invocations, so a
// denial can be proven to short-circuit before the handler ever runs.
func respondELBv2(t *testing.T, nc *nats.Conn, subject, payload string) *int {
	t.Helper()
	calls := 0
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		calls++
		_ = msg.Respond([]byte(payload))
	})
	require.NoError(t, err)
	require.NoError(t, nc.Flush())
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return &calls
}

func normalizeRequestID(s string) string {
	return requestIDPattern.ReplaceAllString(s, "<RequestId>ID</RequestId>")
}

// --- Full-pipeline error envelopes ---------------------------------------

func TestELBv2Pipeline_ErrorEnvelopes_GoldenXML(t *testing.T) {
	cases := []struct {
		name, body, code, message string
		status                    int
	}{
		{"missing action", "", "MissingAction", "The request is missing an action or a required parameter.", http.StatusBadRequest},
		{"unknown action", "Action=Bogus", "InvalidAction", "The action or operation requested is not valid. Verify that the action is typed correctly.", http.StatusBadRequest},
		{"malformed query string", "Action=%zz", "MalformedQueryString", "The query string contains a syntax error.", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, app := elbv2PipelineApp(t, allowAllTaggingDocs(), nil)
			resp := doRequest(app, signELBv2Request(t, tc.body))

			require.Equal(t, tc.status, resp.StatusCode)
			assert.Equal(t, "application/xml", resp.Header.Get("Content-Type"))
			assert.Equal(t, `<?xml version="1.0" encoding="UTF-8"?>
<ErrorResponse>
  <Error>
    <Type>Sender</Type>
    <Code>`+tc.code+`</Code>
    <Message>`+tc.message+`</Message>
  </Error>
  <RequestId>ID</RequestId>
</ErrorResponse>`, normalizeRequestID(readBody(t, resp)))
		})
	}
}

// A handler failure with no AWS code is a 500 InternalError, here because no
// daemon answers the NATS subject.
func TestELBv2Pipeline_NoResponder_500InternalError(t *testing.T) {
	_, app := elbv2PipelineApp(t, allowAllTaggingDocs(), nil)
	resp := doRequest(app, signELBv2Request(t, "Action=DescribeSSLPolicies"))

	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Equal(t, "application/xml", resp.Header.Get("Content-Type"))
	assert.Contains(t, readBody(t, resp), "<Code>InternalError</Code>")
}

// --- Golden success responses ----------------------------------------------

// An action served without NATS answers 200 text/xml in the Query envelope.
func TestELBv2Pipeline_DescribeListenerAttributes_GoldenXML(t *testing.T) {
	_, app := elbv2PipelineApp(t, allowAllTaggingDocs(), nil)
	listener := pipelineListener("web", "1", "2")
	resp := doRequest(app, signELBv2Request(t, "Action=DescribeListenerAttributes&ListenerArn="+listener))

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/xml", resp.Header.Get("Content-Type"))
	assert.Equal(t, `<DescribeListenerAttributesResponse><DescribeListenerAttributesResult><Attributes></Attributes></DescribeListenerAttributesResult></DescribeListenerAttributesResponse>`,
		readBody(t, resp))
}

func TestELBv2Pipeline_DescribeLoadBalancers_GoldenXML(t *testing.T) {
	gw, app := elbv2PipelineApp(t, allowAllTaggingDocs(), nil)
	calls := respondELBv2(t, gw.NATSConn, "elbv2.DescribeLoadBalancers",
		`{"LoadBalancers":[{"LoadBalancerName":"web","LoadBalancerArn":"`+pipelineLB("web", "abc")+`","Type":"application"}]}`)
	resp := doRequest(app, signELBv2Request(t, "Action=DescribeLoadBalancers"))

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/xml", resp.Header.Get("Content-Type"))
	assert.Equal(t, 1, *calls)
	var out struct {
		LBs []struct {
			ARN  string `xml:"LoadBalancerArn"`
			Name string `xml:"LoadBalancerName"`
			Type string `xml:"Type"`
		} `xml:"DescribeLoadBalancersResult>LoadBalancers>member"`
	}
	// Member field order varies between runs, so the result is pinned decoded.
	require.NoError(t, xml.Unmarshal([]byte(readBody(t, resp)), &out))
	require.Len(t, out.LBs, 1)
	assert.Equal(t, pipelineLB("web", "abc"), out.LBs[0].ARN)
	assert.Equal(t, "web", out.LBs[0].Name)
	assert.Equal(t, "application", out.LBs[0].Type)
}

func TestELBv2Pipeline_DescribeAccountLimits_GoldenXML(t *testing.T) {
	_, app := elbv2PipelineApp(t, allowAllTaggingDocs(), nil)
	resp := doRequest(app, signELBv2Request(t, "Action=DescribeAccountLimits"))

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/xml", resp.Header.Get("Content-Type"))
	var out struct {
		Limits []struct {
			Name string `xml:"Name"`
			Max  int    `xml:"Max"`
		} `xml:"DescribeAccountLimitsResult>Limits>member"`
	}
	require.NoError(t, xml.Unmarshal([]byte(readBody(t, resp)), &out))
	got := map[string]int{}
	for _, l := range out.Limits {
		got[l.Name] = l.Max
	}
	// Member field order varies between runs, so the limits are pinned decoded.
	assert.Equal(t, map[string]int{
		"application-load-balancers": 50, "network-load-balancers": 50, "target-groups": 3000,
		"target-groups-per-application-load-balancer": 100, "target-groups-per-action-on-application-load-balancer": 5,
		"target-groups-per-action-on-network-load-balancer": 1, "listeners-per-application-load-balancer": 50,
		"listeners-per-network-load-balancer": 50, "rules-per-application-load-balancer": 100,
		"condition-values-per-alb-rule": 5, "condition-wildcards-per-alb-rule": 5,
		"targets-per-application-load-balancer": 1000, "targets-per-network-load-balancer": 3000,
		"targets-per-availability-zone-per-network-load-balancer": 500,
		"certificates-per-application-load-balancer":              25, "certificates-per-network-load-balancer": 25,
	}, got)
}

// --- Account quota on CreateLoadBalancer -------------------------------------

func TestELBv2Pipeline_CreateLoadBalancer_QuotaExceededStopsBeforeCreate(t *testing.T) {
	quota := admissionquota.New(admissionquota.Limits{Enabled: true, LoadBalancers: 1}, nil)
	gw, app := elbv2PipelineApp(t, allowAllTaggingDocs(), quota)
	respondELBv2(t, gw.NATSConn, "elbv2.DescribeLoadBalancers",
		`{"LoadBalancers":[{"LoadBalancerName":"web","LoadBalancerArn":"`+pipelineLB("web", "abc")+`"}]}`)
	creates := respondELBv2(t, gw.NATSConn, "elbv2.CreateLoadBalancer", `{}`)

	resp := doRequest(app, signELBv2Request(t, "Action=CreateLoadBalancer&Name=second"))

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, readBody(t, resp), "<Code>ResourceLimitExceeded</Code>")
	assert.Zero(t, *creates, "the create must not run once the quota refuses")
}

func TestELBv2Pipeline_CreateLoadBalancer_QuotaDisabledReachesCreate(t *testing.T) {
	gw, app := elbv2PipelineApp(t, allowAllTaggingDocs(), nil)
	respondELBv2(t, gw.NATSConn, "elbv2.DescribeLoadBalancers", `{"LoadBalancers":[]}`)
	creates := respondELBv2(t, gw.NATSConn, "elbv2.CreateLoadBalancer", `{}`)

	resp := doRequest(app, signELBv2Request(t, "Action=CreateLoadBalancer&Name=second"))

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, 1, *creates)
}

// --- Authorization -----------------------------------------------------------

// A denied caller is answered AccessDenied before any NATS handler runs.
func TestELBv2Pipeline_Authz_DeniedStopsBeforeHandler(t *testing.T) {
	docs := scopedTaggingDocs("Allow", "elasticloadbalancing:DeleteLoadBalancer", pipelineLB("other", "zzz"))
	gw, app := elbv2PipelineApp(t, docs, nil)
	calls := respondELBv2(t, gw.NATSConn, "elbv2.DeleteLoadBalancer", `{}`)

	resp := doRequest(app, signELBv2Request(t, "Action=DeleteLoadBalancer&LoadBalancerArn="+pipelineLB("prod", "abc")))

	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	body := readBody(t, resp)
	assert.Contains(t, body, "<Code>AccessDenied</Code>")
	assert.Zero(t, *calls)
}

// The Query input is parsed before authorization, so a request whose list is
// past the parser's cap is MalformedQueryString even for a caller who would be
// denied.
func TestELBv2Pipeline_Authz_ParseFailurePrecedesGate(t *testing.T) {
	docs := scopedTaggingDocs("Allow", "ec2:DescribeTags", "*")
	_, app := elbv2PipelineApp(t, docs, nil)
	var b strings.Builder
	b.WriteString("Action=AddTags")
	for i := 1; i <= 100000; i++ {
		b.WriteString("&ResourceArns.member." + strconv.Itoa(i) + "=x")
	}
	req := signELBv2Request(t, b.String())
	resp := doRequest(app, req)

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, readBody(t, resp), "<Code>MalformedQueryString</Code>")
}

// An unrelated grant denies, and an unknown action is rejected before the gate.
func TestELBv2Pipeline_Authz_UnrelatedGrantDeniedUnknownActionNot(t *testing.T) {
	_, app := elbv2PipelineApp(t, scopedTaggingDocs("Allow", "ec2:DescribeTags", "*"), nil)

	resp := doRequest(app, signELBv2Request(t, "Action=DescribeLoadBalancers"))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	resp = doRequest(app, signELBv2Request(t, "Action=Bogus"))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, readBody(t, resp), "<Code>InvalidAction</Code>")
}

// --- Unavailable dependencies -------------------------------------------------

// A nil NATS connection fails at the request-level gate, before dispatch.
func TestELBv2Pipeline_NilNATS_503ServiceUnavailable(t *testing.T) {
	gw := withELBv2(&GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService(), Credentials: credverify.New(allowAllIAMService(), nil)})

	req := elbv2Request(authzAccountID, "Action=DescribeLoadBalancers")
	w := httptest.NewRecorder()
	gw.Request(w, req)

	resp := w.Result()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Equal(t, "application/xml", resp.Header.Get("Content-Type"))
	assert.Contains(t, normalizeRequestID(readBody(t, resp)), "<Code>ServiceUnavailable</Code>")
}

// --- Oversized body over HTTP -------------------------------------------------

func TestELBv2Pipeline_OversizedBody_413OverHTTP(t *testing.T) {
	handler := setupTestApp(testAccessKey, testSecretKey)

	oversized := bytes.Repeat([]byte("x"), int(sigv4.MaxPayloadLen)+1)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(oversized))
	req.Host = "localhost:9999"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	signTestRequestAs(t, req, oversized, testAccessKey, testSecretKey, "elasticloadbalancing", time.Now().UTC())

	resp := doRequest(handler, req)

	require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
	assert.Equal(t, "application/xml", resp.Header.Get("Content-Type"))
	assert.Contains(t, readBody(t, resp), "<Code>RequestEntityTooLarge</Code>")
}

// --- Pipeline metadata ---------------------------------------------------------

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

func TestELBv2Pipeline_RequestAudit_RecordsResolvedAction(t *testing.T) {
	gw, _ := elbv2PipelineApp(t, allowAllTaggingDocs(), nil)

	var got *audit.Record
	r := chi.NewRouter()
	r.Use(audit.Middleware)
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
			got = audit.From(r.Context())
		})
	})
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", gw.Request)

	listener := pipelineListener("web", "1", "2")
	doRequest(r, signELBv2Request(t, "Action=DescribeListenerAttributes&ListenerArn="+listener))

	require.NotNil(t, got)
	assert.Equal(t, "DescribeListenerAttributes", auditField(got, "action"))
}

// The pipeline signs in testRegion, so ARNs must carry it to pass the
// endpoint-region check.
func pipelineLB(name, id string) string {
	return arn.FormatELBv2LoadBalancer(testRegion, authzAccountID, name, id, "application")
}

func pipelineListener(lbName, lbID, id string) string {
	return arn.FormatELBv2Listener(testRegion, authzAccountID, lbName, lbID, id, "application")
}
