// Characterization tests for Resource Groups Tagging API dispatch as it exists
// in tagging.go today, pinned ahead of moving dispatch out of gateway into a
// registration seam. These assert current behaviour, not AWS parity.

package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/mulgadc/bluebottle/pkg/sigv4"
	"github.com/mulgadc/spinifex/internal/testkit"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/domains/tagging"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/audit"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/envelope"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

const taggingTargetPrefix = "ResourceGroupsTaggingAPI_20170126."

// fakeTaggedLister serves fixed resources and counts calls, so a denial can be
// proven to short-circuit before any lister runs.
type fakeTaggedLister struct {
	resources []tagging.TaggedResource
	err       error
	calls     int
}

func (f *fakeTaggedLister) ListTagged(_ context.Context, _ string, _ map[string]bool) ([]tagging.TaggedResource, error) {
	f.calls++
	return f.resources, f.err
}

// taggingPipelineApp wires SigV4AuthMiddleware -> Request as SetupRoutes does,
// on live test NATS, with docs as every principal's policy.
func taggingPipelineApp(t *testing.T, docs []iamdomain.PolicyDocument, agg *tagging.Aggregator) (*GatewayConfig, http.Handler) {
	t.Helper()
	encryptedSecret, err := iamdomain.EncryptSecret(testSecretKey, testMasterKey)
	require.NoError(t, err)
	_, nc := testutil.StartTestNATS(t)

	gw := &GatewayConfig{
		DisableLogging: true,
		Region:         testRegion,
		NATSConn:       nc,
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
	}
	withTagging(gw, agg)

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", gw.Request)
	return gw, r
}

func allowAllTaggingDocs() []iamdomain.PolicyDocument {
	return []iamdomain.PolicyDocument{{
		Version: "2012-10-17",
		Statement: []iamdomain.Statement{
			{Effect: "Allow", Action: iamdomain.StringOrArr{"*"}, Resource: iamdomain.StringOrArr{"*"}},
		},
	}}
}

func scopedTaggingDocs(effect, action, resource string) []iamdomain.PolicyDocument {
	return []iamdomain.PolicyDocument{{
		Version: "2012-10-17",
		Statement: []iamdomain.Statement{
			{Effect: effect, Action: iamdomain.StringOrArr{action}, Resource: iamdomain.StringOrArr{resource}},
		},
	}}
}

// signTaggingRequest builds a POST / signed with credential-scope service
// "tagging", the shape every Resource Groups Tagging API client sends.
func signTaggingRequest(t *testing.T, target string, body []byte) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Host = "localhost:9999"
	if target != "" {
		req.Header.Set("X-Amz-Target", target)
	}
	signTestRequestAs(t, req, body, testAccessKey, testSecretKey, "tagging", time.Now().UTC())
	return req
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(b)
}

func taggingFixture() (*fakeTaggedLister, *fakeTaggedLister) {
	elb := &fakeTaggedLister{resources: []tagging.TaggedResource{
		{ARN: "arn:aws:elasticloadbalancing:" + testRegion + ":" + authzAccountID + ":loadbalancer/app/web/1", Tags: map[string]string{"env": "prod", "team": "a"}},
	}}
	ec2 := &fakeTaggedLister{resources: []tagging.TaggedResource{
		{ARN: "arn:aws:ec2:" + testRegion + ":" + authzAccountID + ":instance/i-1", Tags: map[string]string{"env": "dev"}},
		{ARN: "arn:aws:ec2:" + testRegion + ":" + authzAccountID + ":volume/vol-1"},
	}}
	return elb, ec2
}

// --- Full-pipeline error envelopes ---------------------------------------

func TestTaggingPipeline_MissingTarget_400MissingActionException(t *testing.T) {
	_, app := taggingPipelineApp(t, allowAllTaggingDocs(), tagging.NewAggregator())
	resp := doRequest(app, signTaggingRequest(t, "", []byte("{}")))

	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, envelope.JSONContentType, resp.Header.Get("Content-Type"))
	assert.Equal(t, "MissingActionException", resp.Header.Get("X-Amzn-Errortype"))
	assert.Equal(t, "MissingActionException", decodeJSONError(t, resp).Type)
}

func TestTaggingPipeline_UnknownAction_400InvalidActionException(t *testing.T) {
	_, app := taggingPipelineApp(t, allowAllTaggingDocs(), tagging.NewAggregator())
	resp := doRequest(app, signTaggingRequest(t, taggingTargetPrefix+"TagResources", []byte("{}")))

	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, envelope.JSONContentType, resp.Header.Get("Content-Type"))
	assert.Equal(t, "InvalidActionException", resp.Header.Get("X-Amzn-Errortype"))
	assert.Equal(t, "InvalidActionException", decodeJSONError(t, resp).Type)
}

// A malformed GetResources body reaches the aggregator and fails there.
func TestTaggingPipeline_MalformedBody_ErrorEnvelope(t *testing.T) {
	_, app := taggingPipelineApp(t, allowAllTaggingDocs(), tagging.NewAggregator())
	resp := doRequest(app, signTaggingRequest(t, taggingTargetPrefix+"GetResources", []byte("{not json")))

	assert.Equal(t, envelope.JSONContentType, resp.Header.Get("Content-Type"))
	body := decodeJSONError(t, resp)
	assert.Equal(t, resp.Header.Get("X-Amzn-Errortype"), body.Type)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, "InvalidParameterValueException", body.Type)
}

// --- Golden GetResources responses ----------------------------------------

func TestTaggingPipeline_GetResources_GoldenAllResources(t *testing.T) {
	elb, ec2 := taggingFixture()
	_, app := taggingPipelineApp(t, allowAllTaggingDocs(), tagging.NewAggregator(elb, ec2))
	resp := doRequest(app, signTaggingRequest(t, taggingTargetPrefix+"GetResources", []byte("{}")))

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, tagging.JSONContentType, resp.Header.Get("Content-Type"))
	// Results come back sorted by ARN, not in lister order.
	assert.JSONEq(t, `{"PaginationToken":"","ResourceTagMappingList":[`+
		`{"ResourceARN":"arn:aws:ec2:`+testRegion+`:`+authzAccountID+`:instance/i-1","Tags":[{"Key":"env","Value":"dev"}]},`+
		`{"ResourceARN":"arn:aws:ec2:`+testRegion+`:`+authzAccountID+`:volume/vol-1","Tags":[]},`+
		`{"ResourceARN":"arn:aws:elasticloadbalancing:`+testRegion+`:`+authzAccountID+`:loadbalancer/app/web/1","Tags":[{"Key":"env","Value":"prod"},{"Key":"team","Value":"a"}]}]}`,
		readBody(t, resp))
}

func TestTaggingPipeline_GetResources_GoldenTagFilter(t *testing.T) {
	elb, ec2 := taggingFixture()
	_, app := taggingPipelineApp(t, allowAllTaggingDocs(), tagging.NewAggregator(elb, ec2))
	body := []byte(`{"TagFilters":[{"Key":"env","Values":["dev"]}]}`)
	resp := doRequest(app, signTaggingRequest(t, taggingTargetPrefix+"GetResources", body))

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.JSONEq(t, `{"PaginationToken":"","ResourceTagMappingList":[`+
		`{"ResourceARN":"arn:aws:ec2:`+testRegion+`:`+authzAccountID+`:instance/i-1","Tags":[{"Key":"env","Value":"dev"}]}]}`,
		readBody(t, resp))
}

func TestTaggingPipeline_GetResources_GoldenPagination(t *testing.T) {
	elb, ec2 := taggingFixture()
	_, app := taggingPipelineApp(t, allowAllTaggingDocs(), tagging.NewAggregator(elb, ec2))
	body := []byte(`{"ResourcesPerPage":1}`)
	resp := doRequest(app, signTaggingRequest(t, taggingTargetPrefix+"GetResources", body))

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.JSONEq(t, `{"PaginationToken":"1","ResourceTagMappingList":[`+
		`{"ResourceARN":"arn:aws:ec2:`+testRegion+`:`+authzAccountID+`:instance/i-1","Tags":[{"Key":"env","Value":"dev"}]}]}`,
		readBody(t, resp))
}

// A lister error surfaces with its own awserrors code through the envelope.
func TestTaggingPipeline_GetResources_ListerErrorPropagates(t *testing.T) {
	failing := &fakeTaggedLister{err: errors.New(awserrors.ErrorServerInternal)}
	_, app := taggingPipelineApp(t, allowAllTaggingDocs(), tagging.NewAggregator(failing))
	resp := doRequest(app, signTaggingRequest(t, taggingTargetPrefix+"GetResources", []byte("{}")))

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Equal(t, envelope.JSONContentType, resp.Header.Get("Content-Type"))
	assert.Equal(t, "ServerInternalException", decodeJSONError(t, resp).Type)
}

// --- Target parsing and telemetry ------------------------------------------

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
			gw := withTagging(&GatewayConfig{}, tagging.NewAggregator())
			assert.Equal(t, tc.wantAction, gw.selector().ResolveAction(req, "tagging"))
			assert.Equal(t, tc.wantAction, resolveNonQueryAction(req, "tagging"))
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

func TestTaggingPipeline_RequestAudit_RecordsResolvedAction(t *testing.T) {
	gw, _ := taggingPipelineApp(t, allowAllTaggingDocs(), tagging.NewAggregator())

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

	doRequest(r, signTaggingRequest(t, taggingTargetPrefix+"GetResources", []byte("{}")))

	require.NotNil(t, got)
	assert.Equal(t, "GetResources", auditField(got, "action"))
}

// --- Oversized body over HTTP ----------------------------------------------

func TestTaggingPipeline_OversizedBody_413OverHTTP(t *testing.T) {
	handler := setupTestApp(testAccessKey, testSecretKey)

	oversized := bytes.Repeat([]byte("x"), int(sigv4.MaxPayloadLen)+1)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(oversized))
	req.Host = "localhost:9999"
	req.Header.Set("X-Amz-Target", taggingTargetPrefix+"GetResources")
	signTestRequestAs(t, req, oversized, testAccessKey, testSecretKey, "tagging", time.Now().UTC())

	resp := doRequest(handler, req)

	require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
	assert.Equal(t, envelope.JSONContentType, resp.Header.Get("Content-Type"))
	assert.Equal(t, "RequestEntityTooLargeException", decodeJSONError(t, resp).Type)
}

// --- Unavailable dependencies -----------------------------------------------

// A nil NATS connection fails at the request-level gate, before dispatch.
func TestRequest_ClusterUnavailableNilConn_Tagging(t *testing.T) {
	gw := withTagging(&GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()}, tagging.NewAggregator())

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Amz-Target", taggingTargetPrefix+"GetResources")
	req = req.WithContext(context.WithValue(req.Context(), ctxService, "tagging"))

	w := httptest.NewRecorder()
	gw.Request(w, req)
	resp := w.Result()

	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Equal(t, envelope.JSONContentType, resp.Header.Get("Content-Type"))
	assert.Equal(t, "ServiceUnavailableException", decodeJSONError(t, resp).Type)
}

// A connected gateway with no aggregator wired answers ServerInternal.
func TestTaggingPipeline_NoAggregator_500ServerInternal(t *testing.T) {
	_, app := taggingPipelineApp(t, allowAllTaggingDocs(), nil)
	resp := doRequest(app, signTaggingRequest(t, taggingTargetPrefix+"GetResources", []byte("{}")))

	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Equal(t, envelope.JSONContentType, resp.Header.Get("Content-Type"))
	assert.Equal(t, "ServerInternalException", decodeJSONError(t, resp).Type)
}

// --- Authorization ----------------------------------------------------------

// Every tagging action is account-level: a grant scoped to a resource ARN does
// not reach it, and a wildcard grant does.
func TestTaggingPipeline_Authz_ScopedAllowDenied_WildcardPermitted(t *testing.T) {
	scoped := scopedTaggingDocs("Allow", "tagging:GetResources", "arn:aws:ec2:"+testRegion+":"+authzAccountID+":instance/i-dev")
	lister := &fakeTaggedLister{}
	_, app := taggingPipelineApp(t, scoped, tagging.NewAggregator(lister))
	resp := doRequest(app, signTaggingRequest(t, taggingTargetPrefix+"GetResources", []byte("{}")))

	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Equal(t, "AccessDeniedException", decodeJSONError(t, resp).Type)
	assert.Zero(t, lister.calls, "the aggregator must not run once the gate denies")

	_, app = taggingPipelineApp(t, scopedTaggingDocs("Allow", "tagging:GetResources", "*"), tagging.NewAggregator())
	resp = doRequest(app, signTaggingRequest(t, taggingTargetPrefix+"GetResources", []byte("{}")))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// A Deny scoped to a resource ARN must not fire: every tagging action is
// evaluated against "*".
func TestTaggingPipeline_Authz_ScopedDenyDoesNotFire(t *testing.T) {
	docs := []iamdomain.PolicyDocument{{
		Version: "2012-10-17",
		Statement: []iamdomain.Statement{
			{Effect: "Allow", Action: iamdomain.StringOrArr{"tagging:*"}, Resource: iamdomain.StringOrArr{"*"}},
			{Effect: "Deny", Action: iamdomain.StringOrArr{"tagging:GetResources"}, Resource: iamdomain.StringOrArr{"arn:aws:ec2:" + testRegion + ":" + authzAccountID + ":instance/i-prod"}},
		},
	}}
	_, app := taggingPipelineApp(t, docs, tagging.NewAggregator())
	resp := doRequest(app, signTaggingRequest(t, taggingTargetPrefix+"GetResources", []byte("{}")))

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// An unrelated action grant does not carry to GetResources, and an explicit
// Deny on the action wins over a wildcard Allow.
func TestTaggingPipeline_Authz_UnrelatedGrantAndExplicitDeny(t *testing.T) {
	_, app := taggingPipelineApp(t, scopedTaggingDocs("Allow", "ec2:DescribeTags", "*"), tagging.NewAggregator())
	resp := doRequest(app, signTaggingRequest(t, taggingTargetPrefix+"GetResources", []byte("{}")))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	docs := []iamdomain.PolicyDocument{{
		Version: "2012-10-17",
		Statement: []iamdomain.Statement{
			{Effect: "Allow", Action: iamdomain.StringOrArr{"*"}, Resource: iamdomain.StringOrArr{"*"}},
			{Effect: "Deny", Action: iamdomain.StringOrArr{"tagging:GetResources"}, Resource: iamdomain.StringOrArr{"*"}},
		},
	}}
	_, app = taggingPipelineApp(t, docs, tagging.NewAggregator())
	resp = doRequest(app, signTaggingRequest(t, taggingTargetPrefix+"GetResources", []byte("{}")))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// An unknown action is rejected before authorization: with no grant at all it
// still answers InvalidAction rather than AccessDenied.
func TestTaggingPipeline_Authz_UnknownActionRejectedBeforeGate(t *testing.T) {
	_, app := taggingPipelineApp(t, scopedTaggingDocs("Allow", "ec2:DescribeTags", "*"), tagging.NewAggregator())
	resp := doRequest(app, signTaggingRequest(t, taggingTargetPrefix+"TagResources", []byte("{}")))

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, "InvalidActionException", decodeJSONError(t, resp).Type)
}

// A denied request is answered AccessDenied even when its body is also bad:
// the gate runs before the body is read.
func TestTaggingPipeline_Authz_DenyPrecedesBodyHandling(t *testing.T) {
	_, app := taggingPipelineApp(t, scopedTaggingDocs("Allow", "ec2:DescribeTags", "*"), tagging.NewAggregator())
	resp := doRequest(app, signTaggingRequest(t, taggingTargetPrefix+"GetResources", []byte(strings.Repeat("{", 8))))

	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Equal(t, "AccessDeniedException", decodeJSONError(t, resp).Type)
}
