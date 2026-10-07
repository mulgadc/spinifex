// Characterization tests for ECR control-plane dispatch as it exists in
// ecrapi.go/ecr.go today, pinned ahead of moving dispatch out of gateway into
// a registration seam. These assert current behaviour, not AWS parity.

package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/mulgadc/bluebottle/pkg/sigv4"
	"github.com/mulgadc/spinifex/internal/testkit"
	handlers_ecr "github.com/mulgadc/spinifex/spinifex/domains/ecr"
	awsapi "github.com/mulgadc/spinifex/spinifex/domains/ecr/awsapi"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/envelope"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// decodeAuthToken decodes an ECR authorizationToken ("AWS:<jwt>") to the jwt.
func decodeAuthToken(token string) (string, error) {
	decoded, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		return "", err
	}
	_, jwtStr, _ := strings.Cut(string(decoded), ":")
	return jwtStr, nil
}

// ecrPipelineApp wires SigV4AuthMiddleware -> Request as SetupRoutes does, on
// live test NATS so Request dispatches. docs is every principal's policy; the
// region is testRegion because signTestRequestAs always signs for it.
func ecrPipelineApp(t *testing.T, docs []handlers_iam.PolicyDocument) (*GatewayConfig, http.Handler) {
	t.Helper()
	encryptedSecret, err := handlers_iam.EncryptSecret(testSecretKey, testMasterKey)
	require.NoError(t, err)
	_, nc := testutil.StartTestNATS(t)

	gw := &GatewayConfig{
		DisableLogging: true,
		Region:         testRegion,
		InternalSuffix: ecrTestSuffix,
		NATSConn:       nc,
		IAMService: &policyMockIAMService{
			mockIAMService: mockIAMService{
				masterKey: testMasterKey,
				accessKeys: map[string]*handlers_iam.AccessKey{
					testAccessKey: {
						AccessKeyID:     testAccessKey,
						SecretAccessKey: encryptedSecret,
						UserName:        "alice",
						AccountID:       ecrTestAccount,
						Status:          "Active",
					},
				},
			},
			getUserPoliciesFn: func(_, _ string) ([]handlers_iam.PolicyDocument, error) { return docs, nil },
		},
	}

	withECR(gw, awsapi.Deps{})

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", gw.Request)
	return gw, r
}

// allowAllECRDocs is the policy ecrPipelineApp uses when the test is not
// exercising authorization itself.
func allowAllECRDocs() []handlers_iam.PolicyDocument {
	return []handlers_iam.PolicyDocument{{
		Version: "2012-10-17",
		Statement: []handlers_iam.Statement{
			{Effect: "Allow", Action: handlers_iam.StringOrArr{"*"}, Resource: handlers_iam.StringOrArr{"*"}},
		},
	}}
}

// signECRRequest builds a POST / signed with credential-scope service "ecr",
// the shape every ECR JSON 1.1 client sends.
func signECRRequest(t *testing.T, target string, body []byte) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Host = "localhost:9999"
	if target != "" {
		req.Header.Set("X-Amz-Target", target)
	}
	signTestRequestAs(t, req, body, testAccessKey, testSecretKey, "ecr", time.Now().UTC())
	return req
}

// jsonErrorEnvelope is the AWS JSON 1.1 error body shape.
type jsonErrorEnvelope struct {
	Type    string `json:"__type"`
	Message string `json:"message"`
}

func decodeJSONError(t *testing.T, resp *http.Response) jsonErrorEnvelope {
	t.Helper()
	var out jsonErrorEnvelope
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}

// --- Full-pipeline error envelopes ---------------------------------------

// TestECRPipeline_MissingTarget_400MissingActionException pins that an ECR
// request with no X-Amz-Target fails at dispatch, not at auth, with the
// MissingAction envelope.
func TestECRPipeline_MissingTarget_400MissingActionException(t *testing.T) {
	_, app := ecrPipelineApp(t, allowAllECRDocs())
	resp := doRequest(app, signECRRequest(t, "", []byte("{}")))

	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, envelope.JSONContentType, resp.Header.Get("Content-Type"))
	assert.Equal(t, "MissingActionException", resp.Header.Get("X-Amzn-Errortype"))
	body := decodeJSONError(t, resp)
	assert.Equal(t, "MissingActionException", body.Type)
}

// TestECRPipeline_UnknownAction_400InvalidActionException pins that a target
// naming an action absent from awsapi.Actions fails with InvalidAction.
func TestECRPipeline_UnknownAction_400InvalidActionException(t *testing.T) {
	_, app := ecrPipelineApp(t, allowAllECRDocs())
	target := awsapi.TargetPrefix + ".MadeUpAction"
	resp := doRequest(app, signECRRequest(t, target, []byte("{}")))

	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, envelope.JSONContentType, resp.Header.Get("Content-Type"))
	assert.Equal(t, "InvalidActionException", resp.Header.Get("X-Amzn-Errortype"))
	body := decodeJSONError(t, resp)
	assert.Equal(t, "InvalidActionException", body.Type)
}

// TestECRPipeline_StubbedAction_501NotImplementedException pins that a
// registered-but-stubbed action (ListRepositories) still answers 501 through
// the full pipeline.
func TestECRPipeline_StubbedAction_501NotImplementedException(t *testing.T) {
	_, app := ecrPipelineApp(t, allowAllECRDocs())
	target := awsapi.TargetPrefix + ".ListRepositories"
	resp := doRequest(app, signECRRequest(t, target, []byte("{}")))

	require.Equal(t, http.StatusNotImplemented, resp.StatusCode)
	assert.Equal(t, envelope.JSONContentType, resp.Header.Get("Content-Type"))
	assert.Equal(t, "NotImplementedException", resp.Header.Get("X-Amzn-Errortype"))
	body := decodeJSONError(t, resp)
	assert.Equal(t, "NotImplementedException", body.Type)
}

// --- Full-pipeline success content type ----------------------------------

// TestECRPipeline_GetAuthorizationToken_200JSONContentType pins that a
// successful ECR action answers 200 with the AWS JSON 1.1 content type
// through the full pipeline.
func TestECRPipeline_GetAuthorizationToken_200JSONContentType(t *testing.T) {
	iss, verify := newECRAuth(t)
	gw, app := ecrPipelineApp(t, allowAllECRDocs())
	gw.ECRTokenIssuer, gw.ECRTokenVerifier = iss, verify
	withECR(gw, awsapi.Deps{AuthorizationToken: awsapi.NewAuthorizationTokenActionService(iss, awsapi.RepositoryEndpoint{
		Region: ecrTestRegion, ServicesDomain: ecrTestSuffix,
	})})

	target := awsapi.TargetPrefix + ".GetAuthorizationToken"
	resp := doRequest(app, signECRRequest(t, target, []byte("{}")))

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, awsapi.JSONContentType, resp.Header.Get("Content-Type"))
}

// --- Target parsing today --------------------------------------------------

// TestECRActionFromTarget_FormsAndTelemetryAgree pins how the ECR
// registration's resolver (dispatch and telemetry/throttle) and the legacy
// resolveNonQueryAction each resolve the X-Amz-Target header today. A refactor
// that lets them diverge breaks this.
func TestECRActionFromTarget_FormsAndTelemetryAgree(t *testing.T) {
	cases := []struct {
		name       string
		target     string
		wantAction string
	}{
		{"service prefix", awsapi.TargetPrefix + ".ListRepositories", "ListRepositories"},
		{"unrelated two-segment prefix", "a.b.ListRepositories", "ListRepositories"},
		{"bare action", "ListRepositories", "ListRepositories"},
		{"trailing dot", awsapi.TargetPrefix + ".", ""},
	}
	ecrEntry, ok := withECR(&GatewayConfig{}, awsapi.Deps{}).registered(awsapi.ServiceName)
	require.True(t, ok)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req.Header.Set("X-Amz-Target", tc.target)
			assert.Equal(t, tc.wantAction, ecrEntry.ResolveAction(req))

			assert.Equal(t, tc.wantAction, resolveNonQueryAction(req, "ecr"),
				"dispatch and telemetry parsers must agree on %q", tc.target)
		})
	}
}

// TestECRRequest_UnrelatedPrefixDispatchesByBareAction pins that a target with
// any "<prefix>.<Action>" shape (not just the ECR prefix) dispatches on the
// bare action name today.
func TestECRRequest_UnrelatedPrefixDispatchesByBareAction(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()}
	err := gw.serveECR(httptest.NewRecorder(), setupECRRequest("a.b.ListRepositories", "{}"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorNotImplemented, err.Error())
}

// TestECRRequest_ECSPrefixedNonECRActionIsInvalidAction pins that a target
// carrying another service's prefix but naming an action outside awsapi.Actions
// fails as an unrecognised ECR action, not as a cross-service error.
func TestECRRequest_ECSPrefixedNonECRActionIsInvalidAction(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()}
	err := gw.serveECR(httptest.NewRecorder(),
		setupECRRequest("AmazonEC2ContainerServiceV20141113.ListClusters", "{}"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAction, err.Error())
}

// TestECRRequest_TrailingDotIsMissingAction pins that a target ending in "."
// resolves to an empty action, the same as an absent target.
func TestECRRequest_TrailingDotIsMissingAction(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()}
	err := gw.serveECR(httptest.NewRecorder(),
		setupECRRequest(awsapi.TargetPrefix+".", "{}"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingAction, err.Error())
}

// --- Exact stub inventory --------------------------------------------------

// TestECRInventory_StubbedSetIsPinned pins the exact set of actions still
// bound to awsapi.NotImplemented, independent of whether a composed capability
// intercepts them before dispatch reaches that stub.
func TestECRInventory_StubbedSetIsPinned(t *testing.T) {
	want := []string{
		"GetAuthorizationToken", "CreateRepository", "DeleteRepository", "DescribeRepositories",
		"ListRepositories", "BatchGetImage", "BatchCheckLayerAvailability", "BatchDeleteImage",
		"PutImage", "ListImages", "DescribeImages", "GetDownloadUrlForLayer", "InitiateLayerUpload",
		"UploadLayerPart", "CompleteLayerUpload", "GetRegistryPolicy", "PutRegistryPolicy",
		"StartLifecyclePolicyPreview", "GetLifecyclePolicyPreview",
		"PutReplicationConfiguration", "ReplicateImage",
	}
	assert.ElementsMatch(t, want, awsapi.StubbedActionNames())
}

// TestECRInventory_ReachableStubsAnswer501 pins the subset of the raw stub set
// that nothing intercepts ahead of the handler table, i.e. what the HTTP
// pipeline actually answers 501 for today.
func TestECRInventory_ReachableStubsAnswer501(t *testing.T) {
	reachableStubs := AWSOperationInventory(ecrRegistrationInventory())["ecr"].Stubbed
	want := []string{
		"ListRepositories", "BatchCheckLayerAvailability", "GetDownloadUrlForLayer",
		"InitiateLayerUpload", "UploadLayerPart", "CompleteLayerUpload", "GetRegistryPolicy",
		"PutRegistryPolicy", "PutReplicationConfiguration", "ReplicateImage",
	}
	assert.ElementsMatch(t, want, reachableStubs)

	gw := &GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()}
	for _, action := range reachableStubs {
		t.Run(action, func(t *testing.T) {
			err := gw.serveECR(httptest.NewRecorder(),
				setupECRRequest(awsapi.TargetPrefix+"."+action, "{}"))
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorNotImplemented, err.Error())
		})
	}
}

// TestECRInventory_ScanningActionsReturnOperationNotSupported pins that every
// unsupported scanning action answers OperationNotSupportedException (400),
// not NotImplemented (501).
func TestECRInventory_ScanningActionsReturnOperationNotSupported(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()}
	for _, action := range awsapi.UnsupportedActionNames() {
		t.Run(action, func(t *testing.T) {
			err := gw.serveECR(httptest.NewRecorder(),
				setupECRRequest(awsapi.TargetPrefix+"."+action, "{}"))
			require.Error(t, err)
			code, _, ok := awserrors.ResolveErrorDetail(err)
			require.True(t, ok)
			assert.Equal(t, awserrors.ErrorOperationNotSupported, code)
		})
	}
}

// --- Oversized body over HTTP ---------------------------------------------

// TestECRPipeline_OversizedBody_413OverHTTP pins that an ECR-signed request
// whose body exceeds the SigV4 cap is rejected inside SigV4AuthMiddleware,
// before ECR dispatch ever runs, with the JSON error envelope.
func TestECRPipeline_OversizedBody_413OverHTTP(t *testing.T) {
	handler := setupTestApp(testAccessKey, testSecretKey)

	oversized := bytes.Repeat([]byte("x"), int(sigv4.MaxPayloadLen)+1)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(oversized))
	req.Host = "localhost:9999"
	req.Header.Set("X-Amz-Target", awsapi.TargetPrefix+".ListRepositories")
	signTestRequestAs(t, req, oversized, testAccessKey, testSecretKey, "ecr", time.Now().UTC())

	resp := doRequest(handler, req)

	require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
	assert.Equal(t, envelope.JSONContentType, resp.Header.Get("Content-Type"))
	body := decodeJSONError(t, resp)
	assert.Equal(t, "RequestEntityTooLargeException", body.Type)
}

// --- Authorization gate before a composed action --------------------------

// recordingRepoStore is a RepositoryStore fake that records whether it was
// ever invoked, so a denial can be proven to short-circuit before the composed
// capability runs.
type recordingRepoStore struct {
	calls int
}

func (s *recordingRepoStore) GetRepo(context.Context, string, string) (handlers_ecr.RepoMeta, error) {
	s.calls++
	return handlers_ecr.RepoMeta{}, errors.New("recordingRepoStore: unexpected call")
}

func (s *recordingRepoStore) ListRepos(context.Context, string) ([]string, error) {
	s.calls++
	return nil, errors.New("recordingRepoStore: unexpected call")
}

func (s *recordingRepoStore) PutRepo(context.Context, string, handlers_ecr.RepoMeta) error {
	s.calls++
	return errors.New("recordingRepoStore: unexpected call")
}

func (s *recordingRepoStore) ListManifests(context.Context, string, string) ([]string, error) {
	s.calls++
	return nil, errors.New("recordingRepoStore: unexpected call")
}

func (s *recordingRepoStore) DeleteRepo(context.Context, string, string) error {
	s.calls++
	return errors.New("recordingRepoStore: unexpected call")
}

// TestECRPipeline_AuthzGateBlocksComposedAction pins that the policy gate in
// ECR dispatch runs, and denies, before a composed capability (here
// RepositoryActionService backing CreateRepository) is ever invoked.
func TestECRPipeline_AuthzGateBlocksComposedAction(t *testing.T) {
	scopedDocs := []handlers_iam.PolicyDocument{{
		Version: "2012-10-17",
		Statement: []handlers_iam.Statement{
			{
				Effect:   "Allow",
				Action:   handlers_iam.StringOrArr{"ecr:CreateRepository"},
				Resource: handlers_iam.StringOrArr{"arn:aws:ecr:" + testRegion + ":" + ecrTestAccount + ":repository/other"},
			},
		},
	}}
	gw, app := ecrPipelineApp(t, scopedDocs)
	store := &recordingRepoStore{}
	withECR(gw, awsapi.Deps{Repository: awsapi.NewRepositoryActionService(store, awsapi.RepositoryEndpoint{
		Region: ecrTestRegion, ServicesDomain: ecrTestSuffix,
	})})

	target := awsapi.TargetPrefix + ".CreateRepository"
	resp := doRequest(app, signECRRequest(t, target, []byte(`{"repositoryName":"team/app"}`)))

	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	body := decodeJSONError(t, resp)
	assert.Equal(t, "AccessDeniedException", body.Type)
	assert.Equal(t, 0, store.calls, "composed capability must not run once the gate denies")
}

// --- GetAuthorizationToken ignores request-supplied identity -------------

// A registryIds field naming a foreign account must not reach the token: its
// account and subject come from the verified caller, never the body.
func TestGetAuthorizationToken_IgnoresRequestSuppliedIdentity(t *testing.T) {
	iss, verify := newECRAuth(t)
	endpoint := awsapi.RepositoryEndpoint{Region: ecrTestRegion, ServicesDomain: ecrTestSuffix}
	gw := withECR(&GatewayConfig{
		Region: ecrTestRegion, InternalSuffix: ecrTestSuffix,
		ECRTokenIssuer: iss, ECRTokenVerifier: verify, DisableLogging: true,
		IAMService: allowAllIAMService(),
	}, awsapi.Deps{AuthorizationToken: awsapi.NewAuthorizationTokenActionService(iss, endpoint)})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"registryIds":["999999999999"]}`))
	ctx := context.WithValue(req.Context(), ctxAccountID, ecrTestAccount)
	ctx = context.WithValue(ctx, ctxPrincipalType, principalTypeUser)
	ctx = context.WithValue(ctx, ctxIdentity, "dev")
	ctx = context.WithValue(ctx, ctxAccessKey, "AKIAGETAUTHTOKENTEST1")
	req.Header.Set("X-Amz-Target", awsapi.TargetPrefix+".GetAuthorizationToken")
	w := httptest.NewRecorder()
	require.NoError(t, gw.serveECR(w, req.WithContext(ctx)))
	require.Equal(t, http.StatusOK, w.Code)

	var out struct {
		AuthorizationData []struct {
			AuthorizationToken string `json:"authorizationToken"`
		} `json:"authorizationData"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.Len(t, out.AuthorizationData, 1)

	decoded, err := decodeAuthToken(out.AuthorizationData[0].AuthorizationToken)
	require.NoError(t, err)
	claims, err := verify.Verify(decoded)
	require.NoError(t, err)
	assert.Equal(t, ecrTestAccount, claims.AccountID)
	assert.Equal(t, "arn:aws:iam::"+ecrTestAccount+":user/dev", claims.Subject)
}

// --- NATS unavailable for ECR ---------------------------------------------

// Stub and composed ECR actions alike fail at the NATS gate, before dispatch
// reads the action.
func TestRequest_ClusterUnavailableNilConn_ECR(t *testing.T) {
	for _, action := range []string{"ListRepositories", "CreateRepository"} {
		t.Run(action, func(t *testing.T) {
			gw := withECR(&GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()}, awsapi.Deps{})

			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req.Header.Set("X-Amz-Target", awsapi.TargetPrefix+"."+action)
			ctx := context.WithValue(req.Context(), ctxService, "ecr")
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

// --- Pipeline metadata for an ECR request ---------------------------------

// TestECRPipeline_TraceActionEnricher_SpanNamedServiceAction pins that the
// server span is renamed to "ecr.<Action>" and carries aws.action once a real
// ECR request runs through SigV4AuthMiddleware and traceActionEnricher.
func TestECRPipeline_TraceActionEnricher_SpanNamedServiceAction(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))
	defer otel.SetTracerProvider(prev)

	gw, _ := ecrPipelineApp(t, allowAllECRDocs())
	r := chi.NewRouter()
	r.Use(otelsetup.HTTPMiddleware("awsgw"))
	r.Use(gw.SigV4AuthMiddleware())
	r.Use(traceActionEnricher)
	r.HandleFunc("/*", gw.Request)

	target := awsapi.TargetPrefix + ".ListRepositories"
	doRequest(r, signECRRequest(t, target, []byte("{}")))

	spans := sr.Ended()
	require.Len(t, spans, 1)
	span := spans[0]
	assert.Equal(t, "ecr.ListRepositories", span.Name())
	got := map[string]string{}
	for _, kv := range span.Attributes() {
		got[string(kv.Key)] = kv.Value.String()
	}
	assert.Equal(t, "ListRepositories", got["aws.action"])
	assert.Equal(t, "ecr", got["aws.service"])
}

// TestECRPipeline_RequestAudit_RecordsResolvedAction pins that the audit
// record's action field carries the resolved ECR action once a real request
// runs through requestAuditMiddleware, SigV4AuthMiddleware and Request.
func TestECRPipeline_RequestAudit_RecordsResolvedAction(t *testing.T) {
	gw, _ := ecrPipelineApp(t, allowAllECRDocs())

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

	target := awsapi.TargetPrefix + ".ListRepositories"
	doRequest(r, signECRRequest(t, target, []byte("{}")))

	require.NotNil(t, got)
	assert.Equal(t, "ListRepositories", got.action)
}

// --- Inventory pin --------------------------------------------------------

// Pins the ECR inventory so a refactor that drops or adds an action, including
// by changing which capability intercepts it, fails here.
func TestECRInventory_RegisteredStubbedUnsupportedArePinned(t *testing.T) {
	ecr := AWSOperationInventory(ecrRegistrationInventory())["ecr"]

	wantRegistered := []string{
		"GetAuthorizationToken", "CreateRepository", "DeleteRepository", "DescribeRepositories",
		"ListRepositories", "PutImageTagMutability", "BatchGetImage", "BatchCheckLayerAvailability",
		"BatchDeleteImage", "PutImage", "ListImages", "DescribeImages", "GetDownloadUrlForLayer",
		"InitiateLayerUpload", "UploadLayerPart", "CompleteLayerUpload", "SetRepositoryPolicy",
		"GetRepositoryPolicy", "DeleteRepositoryPolicy", "GetRegistryPolicy", "PutRegistryPolicy",
		"DescribeRegistry", "PutImageScanningConfiguration", "GetImageScanningConfiguration",
		"StartImageScan", "DescribeImageScanFindings", "GetRegistryScanningConfiguration",
		"PutRegistryScanningConfiguration", "BatchGetRepositoryScanningConfiguration",
		"PutLifecyclePolicy", "GetLifecyclePolicy", "DeleteLifecyclePolicy",
		"StartLifecyclePolicyPreview", "GetLifecyclePolicyPreview", "PutReplicationConfiguration",
		"ReplicateImage", "TagResource", "UntagResource", "ListTagsForResource",
	}
	wantStubbed := []string{
		"ListRepositories", "BatchCheckLayerAvailability", "GetDownloadUrlForLayer",
		"InitiateLayerUpload", "UploadLayerPart", "CompleteLayerUpload", "GetRegistryPolicy",
		"PutRegistryPolicy", "PutReplicationConfiguration", "ReplicateImage",
	}
	wantUnsupported := []string{
		"GetImageScanningConfiguration", "StartImageScan", "DescribeImageScanFindings",
		"GetRegistryScanningConfiguration", "PutRegistryScanningConfiguration",
		"BatchGetRepositoryScanningConfiguration",
	}

	assert.ElementsMatch(t, wantRegistered, ecr.Registered)
	assert.ElementsMatch(t, wantStubbed, ecr.Stubbed)
	assert.ElementsMatch(t, wantUnsupported, ecr.Unsupported)
}
