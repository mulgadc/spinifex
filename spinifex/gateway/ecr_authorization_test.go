package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/stretchr/testify/assert"
)

const (
	ecrAuthzTestAccount = "000000000077"
	ecrAuthzTestUser    = "svc"
)

// ecrAllowPolicy and ecrDenyPolicy build single-statement policy documents for
// the ECR operation-authorization tests below. They are distinct from
// auth_test.go's allowPolicy/allowPolicyResource helpers because those are
// fixed to a single action string; ECR route classification can require
// multiple actions per operation (e.g. cross-repo mount).
func ecrAllowPolicy(actions, resources []string) handlers_iam.PolicyDocument {
	return handlers_iam.PolicyDocument{
		Version: "2012-10-17",
		Statement: []handlers_iam.Statement{
			{Effect: "Allow", Action: actions, Resource: resources},
		},
	}
}

func ecrDenyPolicy(actions, resources []string) handlers_iam.PolicyDocument {
	return handlers_iam.PolicyDocument{
		Version: "2012-10-17",
		Statement: []handlers_iam.Statement{
			{Effect: "Deny", Action: actions, Resource: resources},
		},
	}
}

// ecrAuthzTestGateway builds a GatewayConfig wired for ecrOperationAuthorization
// tests: a non-nil ECRTokenVerifier (so the middleware is active) and an
// ecrMockIAMService seeded with ecrAuthzTestUser's policy documents.
func ecrAuthzTestGateway(t *testing.T, policies []handlers_iam.PolicyDocument) (*GatewayConfig, *ecrMockIAMService) {
	t.Helper()
	_, verify := newECRAuth(t)
	iamSvc := newECRMockIAMService()
	iamSvc.userPolicies[ecrAuthzTestAccount+"|"+ecrAuthzTestUser] = policies
	return &GatewayConfig{
		Region:           ecrTestRegion,
		ECRTokenVerifier: verify,
		IAMService:       iamSvc,
	}, iamSvc
}

// ecrAuthzRequest builds a request carrying a resolved principal, as
// ecrAuthBridge would stash it, for method/path/query.
func ecrAuthzRequest(method, path string, principal principalContext) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	ctx := context.WithValue(req.Context(), ctxECRPrincipal, principal)
	return req.WithContext(ctx)
}

func ecrTestUserPrincipal() principalContext {
	return principalContext{identity: ecrAuthzTestUser, accountID: ecrAuthzTestAccount, principalType: principalTypeUser}
}

// TestECROperationAuthorization_PullOnlyMatrix pins the release-gate positive
// and negative matrix for a PullOnly-shaped policy: pull operations succeed,
// every push/tag-overwrite/delete operation is denied.
func TestECROperationAuthorization_PullOnlyMatrix(t *testing.T) {
	repoARN := "arn:aws:ecr:" + ecrTestRegion + ":" + ecrAuthzTestAccount + ":repository/team/app"
	pullOnly := ecrAllowPolicy(
		[]string{"ecr:GetDownloadUrlForLayer", "ecr:BatchGetImage", "ecr:BatchCheckLayerAvailability"},
		[]string{repoARN},
	)
	gw, _ := ecrAuthzTestGateway(t, []handlers_iam.PolicyDocument{pullOnly})

	allowed := []struct{ method, path string }{
		{http.MethodHead, "/v2/team/app/blobs/sha256:abc"},
		{http.MethodGet, "/v2/team/app/blobs/sha256:abc"},
		{http.MethodHead, "/v2/team/app/manifests/latest"},
		{http.MethodGet, "/v2/team/app/manifests/latest"},
	}
	for _, c := range allowed {
		t.Run("allow "+c.method+" "+c.path, func(t *testing.T) {
			next, called := okHandler()
			w := httptest.NewRecorder()
			gw.ecrOperationAuthorization(next).ServeHTTP(w, ecrAuthzRequest(c.method, c.path, ecrTestUserPrincipal()))
			assert.True(t, *called)
			assert.Equal(t, http.StatusOK, w.Code)
		})
	}

	denied := []struct{ method, path string }{
		{http.MethodPost, "/v2/team/app/blobs/uploads/"},
		{http.MethodPatch, "/v2/team/app/blobs/uploads/upload-1"},
		{http.MethodPut, "/v2/team/app/blobs/uploads/upload-1"},
		{http.MethodDelete, "/v2/team/app/blobs/uploads/upload-1"},
		{http.MethodPut, "/v2/team/app/manifests/latest"},
		{http.MethodDelete, "/v2/team/app/manifests/sha256:abc"},
	}
	for _, c := range denied {
		t.Run("deny "+c.method+" "+c.path, func(t *testing.T) {
			next, called := okHandler()
			w := httptest.NewRecorder()
			gw.ecrOperationAuthorization(next).ServeHTTP(w, ecrAuthzRequest(c.method, c.path, ecrTestUserPrincipal()))
			assert.False(t, *called, "denied operation must never dispatch")
			assert.Equal(t, http.StatusForbidden, w.Code)
		})
	}
}

// TestECROperationAuthorization_ReadOnlyMatrix pins the ReadOnly release-gate
// matrix: catalog, tags, blob and manifest reads succeed; every write/delete
// is denied.
func TestECROperationAuthorization_ReadOnlyMatrix(t *testing.T) {
	repoARN := "arn:aws:ecr:" + ecrTestRegion + ":" + ecrAuthzTestAccount + ":repository/team/app"
	wildcardARN := "arn:aws:ecr:" + ecrTestRegion + ":" + ecrAuthzTestAccount + ":repository/*"
	readOnly := ecrAllowPolicy(
		[]string{
			"ecr:GetDownloadUrlForLayer", "ecr:BatchGetImage", "ecr:BatchCheckLayerAvailability",
			"ecr:ListImages", "ecr:DescribeRepositories",
		},
		[]string{repoARN, wildcardARN},
	)
	gw, _ := ecrAuthzTestGateway(t, []handlers_iam.PolicyDocument{readOnly})

	allowed := []struct{ method, path string }{
		{http.MethodGet, "/v2/_catalog"},
		{http.MethodGet, "/v2/team/app/tags/list"},
		{http.MethodHead, "/v2/team/app/blobs/sha256:abc"},
		{http.MethodGet, "/v2/team/app/manifests/latest"},
	}
	for _, c := range allowed {
		t.Run("allow "+c.method+" "+c.path, func(t *testing.T) {
			next, called := okHandler()
			w := httptest.NewRecorder()
			gw.ecrOperationAuthorization(next).ServeHTTP(w, ecrAuthzRequest(c.method, c.path, ecrTestUserPrincipal()))
			assert.True(t, *called)
			assert.Equal(t, http.StatusOK, w.Code)
		})
	}

	denied := []struct{ method, path string }{
		{http.MethodPost, "/v2/team/app/blobs/uploads/"},
		{http.MethodPut, "/v2/team/app/manifests/latest"},
		{http.MethodDelete, "/v2/team/app/manifests/sha256:abc"},
	}
	for _, c := range denied {
		t.Run("deny "+c.method+" "+c.path, func(t *testing.T) {
			next, called := okHandler()
			w := httptest.NewRecorder()
			gw.ecrOperationAuthorization(next).ServeHTTP(w, ecrAuthzRequest(c.method, c.path, ecrTestUserPrincipal()))
			assert.False(t, *called)
			assert.Equal(t, http.StatusForbidden, w.Code)
		})
	}
}

// TestECROperationAuthorization_RepoScopedAllowDoesNotCrossRepos verifies an
// allow scoped to repository A's ARN does not authorize the identical action
// against repository B.
func TestECROperationAuthorization_RepoScopedAllowDoesNotCrossRepos(t *testing.T) {
	repoAARN := "arn:aws:ecr:" + ecrTestRegion + ":" + ecrAuthzTestAccount + ":repository/team/a"
	policy := ecrAllowPolicy([]string{"ecr:BatchGetImage"}, []string{repoAARN})
	gw, _ := ecrAuthzTestGateway(t, []handlers_iam.PolicyDocument{policy})

	t.Run("repo A allowed", func(t *testing.T) {
		next, called := okHandler()
		w := httptest.NewRecorder()
		gw.ecrOperationAuthorization(next).ServeHTTP(w, ecrAuthzRequest(http.MethodGet, "/v2/team/a/manifests/latest", ecrTestUserPrincipal()))
		assert.True(t, *called)
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("repo B denied", func(t *testing.T) {
		next, called := okHandler()
		w := httptest.NewRecorder()
		gw.ecrOperationAuthorization(next).ServeHTTP(w, ecrAuthzRequest(http.MethodGet, "/v2/team/b/manifests/latest", ecrTestUserPrincipal()))
		assert.False(t, *called)
		assert.Equal(t, http.StatusForbidden, w.Code)
	})
}

// TestECROperationAuthorization_ExplicitDenyWins verifies an explicit Deny
// statement overrides a broader Allow for the same action/resource.
func TestECROperationAuthorization_ExplicitDenyWins(t *testing.T) {
	repoARN := "arn:aws:ecr:" + ecrTestRegion + ":" + ecrAuthzTestAccount + ":repository/team/app"
	allow := ecrAllowPolicy([]string{"ecr:BatchGetImage"}, []string{"arn:aws:ecr:" + ecrTestRegion + ":" + ecrAuthzTestAccount + ":repository/*"})
	deny := ecrDenyPolicy([]string{"ecr:BatchGetImage"}, []string{repoARN})
	gw, _ := ecrAuthzTestGateway(t, []handlers_iam.PolicyDocument{allow, deny})

	next, called := okHandler()
	w := httptest.NewRecorder()
	gw.ecrOperationAuthorization(next).ServeHTTP(w, ecrAuthzRequest(http.MethodGet, "/v2/team/app/manifests/latest", ecrTestUserPrincipal()))
	assert.False(t, *called)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// TestECROperationAuthorization_CrossRepoMount verifies a mount requires both
// destination write and source read permission.
func TestECROperationAuthorization_CrossRepoMount(t *testing.T) {
	destARN := "arn:aws:ecr:" + ecrTestRegion + ":" + ecrAuthzTestAccount + ":repository/team/app"
	sourceARN := "arn:aws:ecr:" + ecrTestRegion + ":" + ecrAuthzTestAccount + ":repository/team/base"

	t.Run("destination write only: denied, never dispatches", func(t *testing.T) {
		gw, _ := ecrAuthzTestGateway(t, []handlers_iam.PolicyDocument{
			ecrAllowPolicy([]string{"ecr:InitiateLayerUpload"}, []string{destARN}),
		})
		next, called := okHandler()
		w := httptest.NewRecorder()
		req := ecrAuthzRequest(http.MethodPost, "/v2/team/app/blobs/uploads/?mount=sha256:abc&from=team/base", ecrTestUserPrincipal())
		gw.ecrOperationAuthorization(next).ServeHTTP(w, req)
		assert.False(t, *called)
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("destination write + source read: allowed", func(t *testing.T) {
		gw, _ := ecrAuthzTestGateway(t, []handlers_iam.PolicyDocument{
			ecrAllowPolicy([]string{"ecr:InitiateLayerUpload"}, []string{destARN}),
			ecrAllowPolicy([]string{"ecr:BatchCheckLayerAvailability"}, []string{sourceARN}),
		})
		next, called := okHandler()
		w := httptest.NewRecorder()
		req := ecrAuthzRequest(http.MethodPost, "/v2/team/app/blobs/uploads/?mount=sha256:abc&from=team/base", ecrTestUserPrincipal())
		gw.ecrOperationAuthorization(next).ServeHTTP(w, req)
		assert.True(t, *called)
		assert.Equal(t, http.StatusOK, w.Code)
	})
}

// TestECROperationAuthorization_UnclassifiedOperationNeverDispatches verifies
// a method/path combination Registry does not implement is refused before
// dispatch rather than silently forwarded.
func TestECROperationAuthorization_UnclassifiedOperationNeverDispatches(t *testing.T) {
	gw, _ := ecrAuthzTestGateway(t, nil)
	next, called := okHandler()
	w := httptest.NewRecorder()
	gw.ecrOperationAuthorization(next).ServeHTTP(w, ecrAuthzRequest(http.MethodPost, "/v2/", ecrTestUserPrincipal()))
	assert.False(t, *called)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// TestECROperationAuthorization_MissingPrincipalUnauthorized verifies the
// middleware fails closed if no principal was stashed on the context (the
// bridge is disabled or was bypassed), rather than dispatching unauthenticated.
func TestECROperationAuthorization_MissingPrincipalUnauthorized(t *testing.T) {
	gw, _ := ecrAuthzTestGateway(t, nil)
	next, called := okHandler()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v2/team/app/manifests/latest", nil)
	gw.ecrOperationAuthorization(next).ServeHTTP(w, req)
	assert.False(t, *called)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// TestECROperationAuthorization_DependencyFailureNeverDispatches verifies an
// IAM backend outage fails closed with 503 rather than allowing or denying
// based on incomplete information.
func TestECROperationAuthorization_DependencyFailureNeverDispatches(t *testing.T) {
	gw, iamSvc := ecrAuthzTestGateway(t, nil)
	iamSvc.genericErr = errors.New(awserrors.ErrorServiceUnavailable)

	next, called := okHandler()
	w := httptest.NewRecorder()
	req := ecrAuthzRequest(http.MethodGet, "/v2/team/app/manifests/latest", ecrTestUserPrincipal())
	gw.ecrOperationAuthorization(next).ServeHTTP(w, req)
	assert.False(t, *called)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

// TestECROperationAuthorization_NilVerifierPassesThrough mirrors the bridge's
// own nil-verifier bypass, used by unit tests of unrelated routes.
func TestECROperationAuthorization_NilVerifierPassesThrough(t *testing.T) {
	gw := &GatewayConfig{}
	next, called := okHandler()
	w := httptest.NewRecorder()
	gw.ecrOperationAuthorization(next).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v2/team/app/manifests/latest", nil))
	assert.True(t, *called)
	assert.Equal(t, http.StatusOK, w.Code)
}

// TestECROperationAuthorization_BasePathNoActionRequired verifies GET /v2/
// dispatches once a principal is resolved, with no resource-specific decision.
func TestECROperationAuthorization_BasePathNoActionRequired(t *testing.T) {
	gw, _ := ecrAuthzTestGateway(t, nil)
	next, called := okHandler()
	w := httptest.NewRecorder()
	req := ecrAuthzRequest(http.MethodGet, "/v2/", ecrTestUserPrincipal())
	gw.ecrOperationAuthorization(next).ServeHTTP(w, req)
	assert.True(t, *called)
	assert.Equal(t, http.StatusOK, w.Code)
}
