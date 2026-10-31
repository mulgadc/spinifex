// Characterization tests for ECR control-plane dispatch as it exists in
// ecrapi.go/ecr.go today, pinned ahead of moving dispatch out of gateway into
// a registration seam. These assert current behaviour, not AWS parity.

package awsgw_test

import (
	"bytes"
	"context"
	"errors"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	"github.com/mulgadc/spinifex/spinifex/gateway"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/reqctx"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/mulgadc/bluebottle/pkg/sigv4"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/ecr"
	awsapi "github.com/mulgadc/spinifex/spinifex/domains/ecr/awsapi"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/audit"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/envelope"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ecrPipelineApp wires SigV4AuthMiddleware -> Request as SetupRoutes does, on
// live test NATS so Request dispatches. docs is every principal's policy; the
// region is gwfix.Region because gwfix.SignRequestAs always signs for it.
func ecrPipelineApp(t *testing.T, docs []iamdomain.PolicyDocument) (*gateway.GatewayConfig, http.Handler) {
	t.Helper()
	encryptedSecret, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
	require.NoError(t, err)
	_, nc := testutil.StartTestNATS(t)

	gw := withVerifier(&gateway.GatewayConfig{
		DisableLogging: true,
		Region:         gwfix.Region,
		InternalSuffix: ecrTestSuffix,
		NATSConn:       nc,
		IAMService: &gwfix.PolicyIAM{
			IAM: gwfix.IAM{
				MasterKey: gwfix.MasterKey,
				AccessKeys: map[string]*iamdomain.AccessKey{
					gwfix.AccessKey: {
						AccessKeyID:     gwfix.AccessKey,
						SecretAccessKey: encryptedSecret,
						UserName:        "alice",
						AccountID:       ecrTestAccount,
						Status:          "Active",
					},
				},
			},
			GetUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) { return docs, nil },
		},
	})

	withECR(gw, awsapi.Deps{})

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", gw.Request)
	return gw, r
}

// allowAllECRDocs is the policy ecrPipelineApp uses when the test is not
// exercising authorization itself.
func allowAllECRDocs() []iamdomain.PolicyDocument {
	return []iamdomain.PolicyDocument{{
		Version: "2012-10-17",
		Statement: []iamdomain.Statement{
			{Effect: "Allow", Action: iamdomain.StringOrArr{"*"}, Resource: iamdomain.StringOrArr{"*"}},
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
	gwfix.SignRequestAs(t, req, body, gwfix.AccessKey, gwfix.SecretKey, "ecr", time.Now().UTC())
	return req
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
	iss, _ := newECRAuth(t)
	gw, app := ecrPipelineApp(t, allowAllECRDocs())
	withECR(gw, awsapi.Deps{AuthorizationToken: awsapi.NewAuthorizationTokenActionService(iss, awsapi.RepositoryEndpoint{
		Region: ecrTestRegion, ServicesDomain: ecrTestSuffix,
	})})

	target := awsapi.TargetPrefix + ".GetAuthorizationToken"
	resp := doRequest(app, signECRRequest(t, target, []byte("{}")))

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, awsapi.JSONContentType, resp.Header.Get("Content-Type"))
}

// --- Target parsing today --------------------------------------------------

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

// --- Oversized body over HTTP ---------------------------------------------

// TestECRPipeline_OversizedBody_413OverHTTP pins that an ECR-signed request
// whose body exceeds the SigV4 cap is rejected inside SigV4AuthMiddleware,
// before ECR dispatch ever runs, with the JSON error envelope.
func TestECRPipeline_OversizedBody_413OverHTTP(t *testing.T) {
	handler := setupTestApp(gwfix.AccessKey, gwfix.SecretKey)

	oversized := bytes.Repeat([]byte("x"), int(sigv4.MaxPayloadLen)+1)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(oversized))
	req.Host = "localhost:9999"
	req.Header.Set("X-Amz-Target", awsapi.TargetPrefix+".ListRepositories")
	gwfix.SignRequestAs(t, req, oversized, gwfix.AccessKey, gwfix.SecretKey, "ecr", time.Now().UTC())

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

func (s *recordingRepoStore) GetRepo(context.Context, string, string) (ecr.RepoMeta, error) {
	s.calls++
	return ecr.RepoMeta{}, errors.New("recordingRepoStore: unexpected call")
}

func (s *recordingRepoStore) ListRepos(context.Context, string) ([]string, error) {
	s.calls++
	return nil, errors.New("recordingRepoStore: unexpected call")
}

func (s *recordingRepoStore) PutRepo(context.Context, string, ecr.RepoMeta) error {
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
	scopedDocs := []iamdomain.PolicyDocument{{
		Version: "2012-10-17",
		Statement: []iamdomain.Statement{
			{
				Effect:   "Allow",
				Action:   iamdomain.StringOrArr{"ecr:CreateRepository"},
				Resource: iamdomain.StringOrArr{"arn:aws:ecr:" + gwfix.Region + ":" + ecrTestAccount + ":repository/other"},
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

// --- NATS unavailable for ECR ---------------------------------------------

// Stub and composed ECR actions alike fail at the NATS gate, before dispatch
// reads the action.
func TestRequest_ClusterUnavailableNilConn_ECR(t *testing.T) {
	for _, action := range []string{"ListRepositories", "CreateRepository"} {
		t.Run(action, func(t *testing.T) {
			gw := withECR(withVerifier(&gateway.GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService()}), awsapi.Deps{})

			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req.Header.Set("X-Amz-Target", awsapi.TargetPrefix+"."+action)
			ctx := reqctx.WithService(req.Context(), "ecr")
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

// TestECRPipeline_RequestAudit_RecordsResolvedAction pins that the audit
// record's action field carries the resolved ECR action once a real request
// runs through audit.Middleware, SigV4AuthMiddleware and Request.
func TestECRPipeline_RequestAudit_RecordsResolvedAction(t *testing.T) {
	gw, _ := ecrPipelineApp(t, allowAllECRDocs())

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

	target := awsapi.TargetPrefix + ".ListRepositories"
	doRequest(r, signECRRequest(t, target, []byte("{}")))

	require.NotNil(t, got)
	assert.Equal(t, "ListRepositories", auditField(got, "action"))
}

// --- Inventory pin --------------------------------------------------------

// Pins the ECR inventory so a refactor that drops or adds an action, including
// by changing which capability intercepts it, fails here.
func TestECRInventory_RegisteredStubbedUnsupportedArePinned(t *testing.T) {
	ecr := gateway.AWSOperationInventory(ecrRegistrationInventory())["ecr"]

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
