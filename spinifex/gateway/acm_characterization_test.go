// Characterization tests for ACM control-plane dispatch as it exists in
// acm.go today, pinned ahead of moving dispatch out of gateway into a
// registration seam. These assert current behaviour, not AWS parity.

package gateway

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/mulgadc/bluebottle/pkg/sigv4"
	"github.com/mulgadc/spinifex/internal/testkit"
	acmawsapi "github.com/mulgadc/spinifex/spinifex/domains/acm/awsapi"
	"github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/envelope"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// acmPipelineApp wires SigV4AuthMiddleware -> Request as SetupRoutes does, on
// live test NATS, with docs as every principal's policy. The region is
// testRegion because signTestRequestAs always signs for it.
func acmPipelineApp(t *testing.T, docs []handlers_iam.PolicyDocument) (*GatewayConfig, http.Handler) {
	t.Helper()
	encryptedSecret, err := handlers_iam.EncryptSecret(testSecretKey, testMasterKey)
	require.NoError(t, err)
	_, nc := testutil.StartTestNATS(t)

	gw := &GatewayConfig{
		DisableLogging: true,
		Region:         testRegion,
		NATSConn:       nc,
		IAMService: &policyMockIAMService{
			mockIAMService: mockIAMService{
				masterKey: testMasterKey,
				accessKeys: map[string]*handlers_iam.AccessKey{
					testAccessKey: {
						AccessKeyID:     testAccessKey,
						SecretAccessKey: encryptedSecret,
						UserName:        "alice",
						AccountID:       authzAccountID,
						Status:          "Active",
					},
				},
			},
			getUserPoliciesFn: func(_, _ string) ([]handlers_iam.PolicyDocument, error) { return docs, nil },
		},
	}

	withACM(gw, acmawsapi.Deps{})

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", gw.Request)
	return gw, r
}

// allowAllACMDocs is the policy acmPipelineApp uses when the test is not
// exercising authorization itself.
func allowAllACMDocs() []handlers_iam.PolicyDocument {
	return []handlers_iam.PolicyDocument{{
		Version: "2012-10-17",
		Statement: []handlers_iam.Statement{
			{Effect: "Allow", Action: handlers_iam.StringOrArr{"*"}, Resource: handlers_iam.StringOrArr{"*"}},
		},
	}}
}

// signACMRequest builds a POST / signed with credential-scope service "acm",
// the shape every ACM JSON 1.1 client sends.
func signACMRequest(t *testing.T, target string, body []byte) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Host = "localhost:9999"
	if target != "" {
		req.Header.Set("X-Amz-Target", target)
	}
	signTestRequestAs(t, req, body, testAccessKey, testSecretKey, "acm", time.Now().UTC())
	return req
}

// subscribeACMCounter answers subject with payload and counts invocations, so
// a denial can be proven to short-circuit before the handler ever runs.
func subscribeACMCounter(t *testing.T, nc *nats.Conn, subject string, payload []byte) *int {
	t.Helper()
	calls := 0
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		calls++
		_ = msg.Respond(payload)
	})
	require.NoError(t, err)
	require.NoError(t, nc.Flush())
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return &calls
}

// --- Full-pipeline error envelopes ---------------------------------------

// TestACMPipeline_MissingTarget_400MissingActionException pins that an ACM
// request with no X-Amz-Target fails at dispatch, not at auth, with the
// MissingAction envelope.
func TestACMPipeline_MissingTarget_400MissingActionException(t *testing.T) {
	_, app := acmPipelineApp(t, allowAllACMDocs())
	resp := doRequest(app, signACMRequest(t, "", []byte("{}")))

	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, envelope.JSONContentType, resp.Header.Get("Content-Type"))
	assert.Equal(t, "MissingActionException", resp.Header.Get("X-Amzn-Errortype"))
	body := decodeJSONError(t, resp)
	assert.Equal(t, "MissingActionException", body.Type)
}

// TestACMPipeline_UnknownAction_400InvalidActionException pins that a target
// naming an action absent from acmActions fails with InvalidAction.
func TestACMPipeline_UnknownAction_400InvalidActionException(t *testing.T) {
	_, app := acmPipelineApp(t, allowAllACMDocs())
	target := "CertificateManager.MadeUpAction"
	resp := doRequest(app, signACMRequest(t, target, []byte("{}")))

	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, envelope.JSONContentType, resp.Header.Get("Content-Type"))
	assert.Equal(t, "InvalidActionException", resp.Header.Get("X-Amzn-Errortype"))
	body := decodeJSONError(t, resp)
	assert.Equal(t, "InvalidActionException", body.Type)
}

// --- Full-pipeline success content type ----------------------------------

// TestACMPipeline_ListCertificates_200JSONContentType pins that a successful
// ACM action answers 200 with the AWS JSON 1.1 content type through the full
// pipeline, once the daemon-side NATS responder answers.
func TestACMPipeline_ListCertificates_200JSONContentType(t *testing.T) {
	gw, app := acmPipelineApp(t, allowAllACMDocs())
	subscribeACMCounter(t, gw.NATSConn, "acm.ListCertificates", []byte(`{}`))

	target := "CertificateManager.ListCertificates"
	resp := doRequest(app, signACMRequest(t, target, []byte("{}")))

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, acmawsapi.JSONContentType, resp.Header.Get("Content-Type"))
}

// --- Target parsing today --------------------------------------------------

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

			assert.Equal(t, tc.wantAction, resolveNonQueryAction(req, "acm"),
				"dispatch and telemetry parsers must agree on %q", tc.target)
		})
	}
}

// --- Oversized body over HTTP ---------------------------------------------

// TestACMPipeline_OversizedBody_413OverHTTP pins that an ACM-signed request
// whose body exceeds the SigV4 cap is rejected inside SigV4AuthMiddleware,
// before ACM dispatch ever runs, with the JSON error envelope.
func TestACMPipeline_OversizedBody_413OverHTTP(t *testing.T) {
	handler := setupTestApp(testAccessKey, testSecretKey)

	oversized := bytes.Repeat([]byte("x"), int(sigv4.MaxPayloadLen)+1)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(oversized))
	req.Host = "localhost:9999"
	req.Header.Set("X-Amz-Target", "CertificateManager.ListCertificates")
	signTestRequestAs(t, req, oversized, testAccessKey, testSecretKey, "acm", time.Now().UTC())

	resp := doRequest(handler, req)

	require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
	assert.Equal(t, envelope.JSONContentType, resp.Header.Get("Content-Type"))
	body := decodeJSONError(t, resp)
	assert.Equal(t, "RequestEntityTooLargeException", body.Type)
}

// --- Authorization gate before the NATS-backed action ----------------------

// TestACMPipeline_AuthzGateBlocksAction pins that the policy gate in ACM
// dispatch runs, and denies, before the NATS-backed action handler is ever
// invoked, through the full pipeline.
func TestACMPipeline_AuthzGateBlocksAction(t *testing.T) {
	scopedDocs := []handlers_iam.PolicyDocument{{
		Version: "2012-10-17",
		Statement: []handlers_iam.Statement{
			{
				Effect:   "Allow",
				Action:   handlers_iam.StringOrArr{"acm:DeleteCertificate"},
				Resource: handlers_iam.StringOrArr{"arn:aws:acm:" + testRegion + ":" + authzAccountID + ":certificate/other"},
			},
		},
	}}
	gw, app := acmPipelineApp(t, scopedDocs)
	calls := subscribeACMCounter(t, gw.NATSConn, "acm.DeleteCertificate", []byte(`{}`))

	target := "CertificateManager.DeleteCertificate"
	body := []byte(`{"CertificateArn":"arn:aws:acm:` + testRegion + `:` + authzAccountID + `:certificate/prod-1111"}`)
	resp := doRequest(app, signACMRequest(t, target, body))

	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	respBody := decodeJSONError(t, resp)
	assert.Equal(t, "AccessDeniedException", respBody.Type)
	assert.Equal(t, 0, *calls, "the NATS-backed handler must not run once the gate denies")
}

// --- NATS unavailable for ACM ---------------------------------------------

// TestRequest_ClusterUnavailableNilConn_ACM pins that ACM fails at the
// request-level NATS gate before dispatch reads the action, so ACM's own
// nil-connection check is unreachable over HTTP.
func TestRequest_ClusterUnavailableNilConn_ACM(t *testing.T) {
	for _, action := range []string{"ListCertificates", "DeleteCertificate"} {
		t.Run(action, func(t *testing.T) {
			gw := withACM(&GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()}, acmawsapi.Deps{})

			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req.Header.Set("X-Amz-Target", "CertificateManager."+action)
			ctx := context.WithValue(req.Context(), ctxService, "acm")
			req = req.WithContext(ctx)

			w := httptest.NewRecorder()
			gw.Request(w, req)
			resp := w.Result()

			assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
			assert.Equal(t, envelope.JSONContentType, resp.Header.Get("Content-Type"))
			body := decodeJSONError(t, resp)
			assert.Equal(t, "ServiceUnavailableException", body.Type)
		})
	}
}

// --- Pipeline metadata for an ACM request ---------------------------------

// TestACMPipeline_TraceActionEnricher_SpanNamedServiceAction pins that the
// server span is renamed to "acm.<Action>" and carries aws.action once a real
// ACM request runs through SigV4AuthMiddleware and traceActionEnricher.
func TestACMPipeline_TraceActionEnricher_SpanNamedServiceAction(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))
	defer otel.SetTracerProvider(prev)

	gw, _ := acmPipelineApp(t, allowAllACMDocs())
	subscribeACMCounter(t, gw.NATSConn, "acm.ListCertificates", []byte(`{}`))

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

// TestACMPipeline_RequestAudit_RecordsResolvedAction pins that the audit
// record's action field carries the resolved ACM action once a real request
// runs through requestAuditMiddleware, SigV4AuthMiddleware and Request.
func TestACMPipeline_RequestAudit_RecordsResolvedAction(t *testing.T) {
	gw, _ := acmPipelineApp(t, allowAllACMDocs())
	subscribeACMCounter(t, gw.NATSConn, "acm.ListCertificates", []byte(`{}`))

	var got *requestAudit
	r := chi.NewRouter()
	r.Use(requestAuditMiddleware)
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
			got = auditFrom(r.Context())
		})
	})
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", gw.Request)

	target := "CertificateManager.ListCertificates"
	doRequest(r, signACMRequest(t, target, []byte("{}")))

	require.NotNil(t, got)
	assert.Equal(t, "ListCertificates", got.action)
}

// --- Inventory pin ----------------------------------------------------------

// TestACMInventory_RegisteredArePinned pins the ACM inventory so a refactor
// that drops or adds an action, including by moving ACM onto the
// registration seam, fails here.
func TestACMInventory_RegisteredArePinned(t *testing.T) {
	acm := AWSOperationInventory(acmRegistrationInventory())["acm"]

	want := []string{
		"ImportCertificate", "RequestCertificate", "DescribeCertificate", "GetCertificate",
		"ListCertificates", "DeleteCertificate", "ListTagsForCertificate", "AddTagsToCertificate",
		"RemoveTagsFromCertificate",
	}
	assert.ElementsMatch(t, want, acm.Registered)
	assert.Empty(t, acm.Stubbed)
	assert.Empty(t, acm.Unsupported)
}
