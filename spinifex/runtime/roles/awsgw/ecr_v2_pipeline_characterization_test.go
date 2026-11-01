package awsgw_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/pipeline"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	ecrauth "github.com/mulgadc/spinifex/spinifex/domains/ecr/auth"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests drive /v2 through the gateway's real handler, so they pin the
// route order, host matching, token authentication, principal rehydration and
// per-operation authorization as one pipeline. They name no gateway-private
// symbol beyond the fixture builder, which is the only thing a move of the
// bridge should have to change.

const (
	v2Account = "000000000042"
	v2Host    = v2Account + ".dkr.ecr." + ecrTestRegion + "." + ecrTestSuffix
	v2AKID    = "AKIAV2PIPELINE0000001"
	v2ASID    = "ASIAV2PIPELINE0000001"
)

type v2Env struct {
	handler http.Handler
	iam     *ecrMockIAMService
	sts     *ecrMockSTSService
	issuer  *ecrauth.Issuer
}

// v2Options tunes the fixture; the zero value is an allow-all user "dev".
type v2Options struct {
	policies []iamdomain.PolicyDocument
}

func newV2Env(t *testing.T, opts v2Options) *v2Env {
	t.Helper()
	iss, verify := newECRAuth(t)
	iamSvc := newECRMockIAMService()
	stsSvc := newECRMockSTSService()
	seedECRTestUser(iamSvc, v2Account, "dev", v2AKID)
	policies := opts.policies
	if policies == nil {
		policies = []iamdomain.PolicyDocument{ecrAllowPolicy([]string{"ecr:*"}, []string{"*"})}
	}
	iamSvc.userPolicies[v2Account+"|dev"] = policies

	gw := withVerifier(gwfix.WithDeps(&pipeline.Config{
		DisableLogging: true,
		Region:         ecrTestRegion,
	}, gwfix.Deps{IAM: iamSvc, STS: stsSvc}))
	gw.PublicRoutes = awsgw.PublicRoutes(nil, newTestRegistryHandler(t, gw, iss, verify))
	return &v2Env{handler: gw.SetupRoutes(), iam: iamSvc, sts: stsSvc, issuer: iss}
}

func (e *v2Env) mint(t *testing.T, account, arn, principalType, akid string) string {
	t.Helper()
	tok, _, err := e.issuer.Mint(ecrauth.Principal{AccountID: account, ARN: arn, Type: principalType, AccessKeyID: akid})
	require.NoError(t, err)
	return tok
}

// mintRole is a role session's token carrying the role's immutable ID, as
// GetAuthorizationToken now issues it.
func (e *v2Env) mintRole(t *testing.T, roleID string) string {
	t.Helper()
	tok, _, err := e.issuer.Mint(ecrauth.Principal{
		AccountID: v2Account, ARN: "arn:aws:sts::" + v2Account + ":assumed-role/deploy/s1",
		Type: gwfix.PrincipalTypeAssumedRole, AccessKeyID: v2ASID, RoleID: roleID,
	})
	require.NoError(t, err)
	return tok
}

func (e *v2Env) userToken(t *testing.T) string {
	return e.mint(t, v2Account, "arn:aws:iam::"+v2Account+":user/dev", gwfix.PrincipalTypeUser, v2AKID)
}

func basic(token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("AWS:"+token))
}

type v2Response struct {
	*httptest.ResponseRecorder
}

func (r v2Response) errorCode(t *testing.T) string {
	t.Helper()
	var body struct {
		Errors []struct {
			Code string `json:"code"`
		} `json:"errors"`
	}
	require.NoError(t, json.Unmarshal(r.Body.Bytes(), &body), r.Body.String())
	require.Len(t, body.Errors, 1, r.Body.String())
	return body.Errors[0].Code
}

func (e *v2Env) do(method, path, host string, authz ...string) v2Response {
	req := httptest.NewRequest(method, path, nil)
	if host != "" {
		req.Host = host
	}
	for _, a := range authz {
		req.Header.Add("Authorization", a)
	}
	w := httptest.NewRecorder()
	e.handler.ServeHTTP(w, req)
	return v2Response{w}
}

const v2Challenge = `Bearer realm="https://` + v2Host + `/v2/token",service="` + v2Host + `"`

func TestV2Pipeline_UnauthenticatedChallengesBeforeSigV4(t *testing.T) {
	e := newV2Env(t, v2Options{})

	for _, path := range []string{"/v2/", "/v2/team/app/tags/list"} {
		w := e.do(http.MethodGet, path, v2Host)

		assert.Equal(t, http.StatusUnauthorized, w.Code, path)
		assert.Equal(t, v2Challenge, w.Header().Get("WWW-Authenticate"), path)
		assert.Equal(t, "UNAUTHORIZED", w.errorCode(t), path)
	}
}

func TestV2Pipeline_VersionCheckAndStub(t *testing.T) {
	e := newV2Env(t, v2Options{})
	token := e.userToken(t)

	for _, authz := range []string{basic(token), "Bearer " + token} {
		w := e.do(http.MethodGet, "/v2/", v2Host, authz)
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "registry/2.0", w.Header().Get("Docker-Distribution-Api-Version"))
	}

	// With no Registry wired an authorized request reaches the 501 stub.
	w := e.do(http.MethodGet, "/v2/team/app/tags/list", v2Host, basic(token))
	assert.Equal(t, http.StatusNotImplemented, w.Code)
	assert.Equal(t, "UNSUPPORTED", w.errorCode(t))
}

func TestV2Pipeline_TokenRejections(t *testing.T) {
	e := newV2Env(t, v2Options{})
	good := e.userToken(t)
	other := e.mint(t, "000000000099", "arn:aws:iam::000000000099:user/dev", gwfix.PrincipalTypeUser, v2AKID)

	tests := []struct {
		name       string
		host       string
		authz      []string
		wantStatus int
		wantCode   string
		challenge  bool
	}{
		{"not a token", v2Host, []string{"Bearer not.a.token"}, http.StatusUnauthorized, "UNAUTHORIZED", true},
		{"unknown scheme", v2Host, []string{"Digest abc"}, http.StatusUnauthorized, "UNAUTHORIZED", true},
		{"basic with wrong user", v2Host, []string{"Basic " + base64.StdEncoding.EncodeToString([]byte("bob:"+good))}, http.StatusUnauthorized, "UNAUTHORIZED", true},
		{"two authorization headers", v2Host, []string{"Bearer a.b.c", "Bearer d.e.f"}, http.StatusBadRequest, "UNAUTHORIZED", false},
		{"token account is not the host account", v2Host, []string{"Bearer " + other}, http.StatusForbidden, "DENIED", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := e.do(http.MethodGet, "/v2/", tt.host, tt.authz...)

			assert.Equal(t, tt.wantStatus, w.Code)
			assert.Equal(t, tt.wantCode, w.errorCode(t))
			if tt.challenge {
				assert.Equal(t, v2Challenge, w.Header().Get("WWW-Authenticate"))
			} else {
				assert.Empty(t, w.Header().Get("WWW-Authenticate"))
			}
		})
	}
}

// A host that is not a registry host carries no target account, so the
// account guard has nothing to compare and the token's own account applies.
func TestV2Pipeline_NonRegistryHostPassesThrough(t *testing.T) {
	e := newV2Env(t, v2Options{})

	w := e.do(http.MethodGet, "/v2/", "gateway.example.test", basic(e.userToken(t)))

	assert.Equal(t, http.StatusOK, w.Code)
}

// The token is a pointer to current IAM/STS state, so each way that state can
// move turns the same valid, unexpired JWT into a 401 or a 503.
func TestV2Pipeline_PrincipalRehydration(t *testing.T) {
	user := func(e *v2Env) string { return e.userToken(t) }
	session := func(e *v2Env) string {
		return e.mint(t, v2Account, "arn:aws:iam::"+v2Account+":user/dev", gwfix.PrincipalTypeUser, v2ASID)
	}
	role := func(e *v2Env) string {
		return e.mint(t, v2Account, "arn:aws:sts::"+v2Account+":assumed-role/deploy/s1", gwfix.PrincipalTypeAssumedRole, v2ASID)
	}
	seedSession := func(e *v2Env) {
		seedECRSessionUser(e.iam, e.sts, v2Account, "dev", v2ASID, time.Now().Add(time.Hour))
	}
	seedRole := func(e *v2Env) {
		seedECRAssumedRole(e.iam, e.sts, v2Account, "deploy", "AROAV2ROLE000001", v2ASID, "s1", time.Now().Add(time.Hour))
		e.iam.rolePolicies[v2Account+"|deploy"] = []iamdomain.PolicyDocument{ecrAllowPolicy([]string{"ecr:*"}, []string{"*"})}
	}

	tests := []struct {
		name       string
		token      func(*v2Env) string
		mutate     func(*v2Env)
		wantStatus int
		wantCode   string
	}{
		{"active user", user, func(*v2Env) {}, http.StatusOK, ""},
		{"user session", session, seedSession, http.StatusOK, ""},
		{"assumed-role session", role, seedRole, http.StatusOK, ""},
		{"access key deactivated", user, func(e *v2Env) {
			e.iam.accessKeys[v2AKID].Status = iamdomain.AccessKeyStatusInactive
		}, http.StatusUnauthorized, "UNAUTHORIZED"},
		{"access key deleted", user, func(e *v2Env) { delete(e.iam.accessKeys, v2AKID) }, http.StatusUnauthorized, "UNAUTHORIZED"},
		{"user deleted", user, func(e *v2Env) { delete(e.iam.users, v2Account+"|dev") }, http.StatusUnauthorized, "UNAUTHORIZED"},
		{"account suspended", user, func(e *v2Env) {
			e.iam.accounts[v2Account].Status = "SUSPENDED"
		}, http.StatusUnauthorized, "UNAUTHORIZED"},
		{"account missing", user, func(e *v2Env) { delete(e.iam.accounts, v2Account) }, http.StatusUnauthorized, "UNAUTHORIZED"},
		{"key belongs to another account", user, func(e *v2Env) {
			e.iam.accessKeys[v2AKID].AccountID = "000000000099"
		}, http.StatusUnauthorized, "UNAUTHORIZED"},
		{"session expired", session, func(e *v2Env) {
			seedSession(e)
			e.sts.sessions[v2ASID].ExpiresAt = time.Now().Add(-time.Minute)
		}, http.StatusUnauthorized, "UNAUTHORIZED"},
		{"session unknown", session, func(*v2Env) {}, http.StatusUnauthorized, "UNAUTHORIZED"},
		{"session principal replaced", session, func(e *v2Env) {
			seedSession(e)
			e.sts.principalErr = stsdomain.ErrSessionPrincipalReplaced
		}, http.StatusUnauthorized, "UNAUTHORIZED"},
		{"unrecognized key prefix", func(e *v2Env) string {
			return e.mint(t, v2Account, "arn:aws:iam::"+v2Account+":user/dev", gwfix.PrincipalTypeUser, "AROAUNKNOWN0000000001")
		}, func(*v2Env) {}, http.StatusUnauthorized, "UNAUTHORIZED"},
		{"claim subject does not match the principal", func(e *v2Env) string {
			return e.mint(t, v2Account, "arn:aws:iam::"+v2Account+":user/someone-else", gwfix.PrincipalTypeUser, v2AKID)
		}, func(*v2Env) {}, http.StatusUnauthorized, "UNAUTHORIZED"},
		{"claim principal type does not match", func(e *v2Env) string {
			return e.mint(t, v2Account, "arn:aws:iam::"+v2Account+":user/dev", gwfix.PrincipalTypeAssumedRole, v2AKID)
		}, func(*v2Env) {}, http.StatusUnauthorized, "UNAUTHORIZED"},
		{"IAM outage", user, func(e *v2Env) { e.iam.genericErr = errors.New("nats: timeout") }, http.StatusServiceUnavailable, "UNKNOWN"},
		{"STS outage", session, func(e *v2Env) {
			seedSession(e)
			e.sts.principalErr = errors.New("kv: unavailable")
		}, http.StatusServiceUnavailable, "UNKNOWN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newV2Env(t, v2Options{})
			tt.mutate(e)

			w := e.do(http.MethodGet, "/v2/", v2Host, basic(tt.token(e)))

			assert.Equal(t, tt.wantStatus, w.Code, w.Body.String())
			if tt.wantCode == "" {
				return
			}
			assert.Equal(t, tt.wantCode, w.errorCode(t))
			if tt.wantStatus == http.StatusUnauthorized {
				assert.Equal(t, v2Challenge, w.Header().Get("WWW-Authenticate"))
			}
		})
	}
}

// AWS keeps an ECR token valid after the session that minted it expires and judges
// the role as it is now: its policy on every request, and its existence.
func TestV2Pipeline_TokenOutlivesItsRoleSession(t *testing.T) {
	const roleID = "AROAV2ROLE000001"
	const manifest = "/v2/team/app/manifests/latest"
	seed := func(e *v2Env) {
		seedECRAssumedRole(e.iam, e.sts, v2Account, "deploy", roleID, v2ASID, "s1", time.Now().Add(-time.Minute))
		e.iam.rolePolicies[v2Account+"|deploy"] = []iamdomain.PolicyDocument{ecrAllowPolicy([]string{"ecr:*"}, []string{"*"})}
	}
	withID := func(e *v2Env) string { return e.mintRole(t, roleID) }
	withoutID := func(e *v2Env) string { return e.mintRole(t, "") }

	tests := []struct {
		name       string
		token      func(*v2Env) string
		mutate     func(*v2Env)
		wantStatus int
		wantCode   string
		challenge  bool
		roleGone   bool
	}{
		{"session expired, role live", withID, seed, http.StatusNotImplemented, "UNSUPPORTED", false, false},
		{"session swept, role live", withID, func(e *v2Env) {
			seed(e)
			delete(e.sts.sessions, v2ASID)
		}, http.StatusNotImplemented, "UNSUPPORTED", false, false},
		{"session expired, role policy removed", withID, func(e *v2Env) {
			seed(e)
			e.iam.rolePolicies[v2Account+"|deploy"] = nil
		}, http.StatusForbidden, "DENIED", false, false},
		{"session expired, role deleted", withID, func(e *v2Env) {
			seed(e)
			delete(e.iam.roles, v2Account+"|deploy")
		}, http.StatusForbidden, "DENIED", false, true},
		{"session expired, role recreated under a new ID", withID, func(e *v2Env) {
			seed(e)
			e.iam.roles[v2Account+"|deploy"].RoleId = aws.String("AROAV2ROLE000002")
		}, http.StatusForbidden, "DENIED", false, true},
		{"session expired, account suspended", withID, func(e *v2Env) {
			seed(e)
			e.iam.accounts[v2Account].Status = "SUSPENDED"
		}, http.StatusUnauthorized, "UNAUTHORIZED", true, false},
		{"session expired, IAM outage", withID, func(e *v2Env) {
			seed(e)
			e.iam.genericErr = errors.New("nats: timeout")
		}, http.StatusServiceUnavailable, "UNKNOWN", false, false},
		{"session expired, token without a role ID", withoutID, seed, http.StatusUnauthorized, "UNAUTHORIZED", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newV2Env(t, v2Options{})
			tt.mutate(e)

			w := e.do(http.MethodGet, manifest, v2Host, basic(tt.token(e)))

			assert.Equal(t, tt.wantStatus, w.Code, w.Body.String())
			assert.Equal(t, tt.wantCode, w.errorCode(t))
			if tt.challenge {
				assert.Equal(t, v2Challenge, w.Header().Get("WWW-Authenticate"))
			} else {
				assert.Empty(t, w.Header().Get("WWW-Authenticate"))
			}
			if tt.roleGone {
				assert.JSONEq(t, `{"errors":[{"code":"DENIED","message":"The security token included in the request is invalid."}]}`, w.Body.String())
			}
		})
	}
}

func TestV2Pipeline_OperationAuthorization(t *testing.T) {
	repoARN := "arn:aws:ecr:" + ecrTestRegion + ":" + v2Account + ":repository/team/app"
	pullOnly := ecrAllowPolicy(
		[]string{"ecr:GetDownloadUrlForLayer", "ecr:BatchGetImage", "ecr:BatchCheckLayerAvailability"},
		[]string{repoARN},
	)

	tests := []struct {
		name       string
		policies   []iamdomain.PolicyDocument
		method     string
		path       string
		wantStatus int
		wantCode   string
	}{
		{"pull allowed (reaches the 501 stub)", []iamdomain.PolicyDocument{pullOnly}, http.MethodGet, "/v2/team/app/manifests/latest", http.StatusNotImplemented, "UNSUPPORTED"},
		{"referrers need BatchGetImage", []iamdomain.PolicyDocument{pullOnly}, http.MethodGet, "/v2/team/app/referrers/sha256:abc", http.StatusNotImplemented, "UNSUPPORTED"},
		{"push denied for a pull-only policy", []iamdomain.PolicyDocument{pullOnly}, http.MethodPut, "/v2/team/app/manifests/latest", http.StatusForbidden, "DENIED"},
		{"blob upload denied for a pull-only policy", []iamdomain.PolicyDocument{pullOnly}, http.MethodPost, "/v2/team/app/blobs/uploads/", http.StatusForbidden, "DENIED"},
		{"other repository denied", []iamdomain.PolicyDocument{pullOnly}, http.MethodGet, "/v2/team/other/manifests/latest", http.StatusForbidden, "DENIED"},
		{"explicit deny wins", []iamdomain.PolicyDocument{
			ecrAllowPolicy([]string{"ecr:*"}, []string{"*"}),
			ecrDenyPolicy([]string{"ecr:BatchGetImage"}, []string{repoARN}),
		}, http.MethodGet, "/v2/team/app/manifests/latest", http.StatusForbidden, "DENIED"},
		{"no policy at all", []iamdomain.PolicyDocument{}, http.MethodGet, "/v2/team/app/manifests/latest", http.StatusForbidden, "DENIED"},
		{"unrecognized operation is never dispatched", nil, http.MethodDelete, "/v2/", http.StatusNotFound, "NAME_UNKNOWN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newV2Env(t, v2Options{policies: tt.policies})

			w := e.do(tt.method, tt.path, v2Host, basic(e.userToken(t)))

			assert.Equal(t, tt.wantStatus, w.Code, w.Body.String())
			assert.Equal(t, tt.wantCode, w.errorCode(t))
		})
	}
}

func TestV2Pipeline_PolicyOutageFailsClosed(t *testing.T) {
	e := newV2Env(t, v2Options{})
	token := basic(e.userToken(t))
	// The rehydrate reads succeed; only the policy read fails.
	e.iam.userPolicyErr = errors.New("nats: timeout")

	w := e.do(http.MethodGet, "/v2/team/app/manifests/latest", v2Host, token)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, "UNKNOWN", w.errorCode(t))
}

func TestV2Pipeline_TokenEndpoint(t *testing.T) {
	t.Run("exchange mints a fresh token for the same principal", func(t *testing.T) {
		e := newV2Env(t, v2Options{})
		old := e.userToken(t)

		for _, authz := range []string{basic(old), "Bearer " + old} {
			w := e.do(http.MethodGet, "/v2/token", v2Host, authz)

			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
			var body struct {
				Token       string `json:"token"`
				AccessToken string `json:"access_token"`
				ExpiresIn   int    `json:"expires_in"`
				IssuedAt    string `json:"issued_at"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			assert.NotEmpty(t, body.Token)
			assert.Equal(t, body.Token, body.AccessToken)
			assert.Positive(t, body.ExpiresIn)
			assert.NotEmpty(t, body.IssuedAt)

			// The fresh token authenticates against the registry.
			again := e.do(http.MethodGet, "/v2/", v2Host, "Bearer "+body.Token)
			assert.Equal(t, http.StatusOK, again.Code)
		}
	})

	t.Run("no credential is a 401 with no challenge", func(t *testing.T) {
		w := newV2Env(t, v2Options{}).do(http.MethodGet, "/v2/token", v2Host)

		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Empty(t, w.Header().Get("WWW-Authenticate"))
		assert.Equal(t, "UNAUTHORIZED", w.errorCode(t))
	})

	t.Run("invalid credential is a 401 with no challenge", func(t *testing.T) {
		w := newV2Env(t, v2Options{}).do(http.MethodGet, "/v2/token", v2Host, "Bearer not.a.token")

		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Empty(t, w.Header().Get("WWW-Authenticate"))
	})

	t.Run("two authorization headers", func(t *testing.T) {
		w := newV2Env(t, v2Options{}).do(http.MethodGet, "/v2/token", v2Host, "Bearer a.b.c", "Bearer d.e.f")

		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("token for another account than the host", func(t *testing.T) {
		e := newV2Env(t, v2Options{})
		other := e.mint(t, "000000000099", "arn:aws:iam::000000000099:user/dev", gwfix.PrincipalTypeUser, v2AKID)

		w := e.do(http.MethodGet, "/v2/token", v2Host, "Bearer "+other)

		assert.Equal(t, http.StatusForbidden, w.Code)
		assert.Equal(t, "DENIED", w.errorCode(t))
	})

	t.Run("a revoked identity is not refreshed", func(t *testing.T) {
		e := newV2Env(t, v2Options{})
		token := e.userToken(t)
		e.iam.accessKeys[v2AKID].Status = iamdomain.AccessKeyStatusInactive

		w := e.do(http.MethodGet, "/v2/token", v2Host, basic(token))

		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Empty(t, w.Header().Get("WWW-Authenticate"))
	})

	t.Run("a dependency outage is a 503", func(t *testing.T) {
		e := newV2Env(t, v2Options{})
		token := e.userToken(t)
		e.iam.genericErr = errors.New("nats: timeout")

		w := e.do(http.MethodGet, "/v2/token", v2Host, basic(token))

		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		assert.Equal(t, "UNKNOWN", w.errorCode(t))
	})
}

// Only the exact /v2 subtree is the registry; the SigV4 surface keeps every
// other path.
func TestV2Pipeline_RouteSplit(t *testing.T) {
	e := newV2Env(t, v2Options{})

	registry := e.do(http.MethodGet, "/v2", v2Host)
	assert.Equal(t, http.StatusUnauthorized, registry.Code)
	assert.Equal(t, v2Challenge, registry.Header().Get("WWW-Authenticate"))

	notRegistry := e.do(http.MethodGet, "/v20/x", v2Host)
	assert.NotEqual(t, v2Challenge, notRegistry.Header().Get("WWW-Authenticate"))
}
