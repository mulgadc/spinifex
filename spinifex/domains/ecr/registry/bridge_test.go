package ecrregistry

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	ecrauth "github.com/mulgadc/spinifex/spinifex/domains/ecr/auth"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	bridgeRegion  = "ap-southeast-2"
	bridgeAccount = "000000000001"
	bridgeAKID    = "AKIABRIDGETESTUSER001"
	bridgeUserARN = "arn:aws:iam::" + bridgeAccount + ":user/dev"
	bridgeHost    = bridgeAccount + ".dkr.ecr." + bridgeRegion + ".spinifex.internal"
)

type targetKey struct{}

// hostMiddleware records the account named by the host's first label, as the
// gateway's host matcher does.
func hostMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account, _, _ := strings.Cut(r.Host, ".")
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), targetKey{}, account)))
	})
}

func hostTarget(r *http.Request) (account, region string, ok bool) {
	account, ok = r.Context().Value(targetKey{}).(string)
	return account, bridgeRegion, ok
}

func noTarget(*http.Request) (string, string, bool) { return "", "", false }
func noIP(*http.Request) string                     { return "" }

type fakeRehydrator struct {
	principal Principal
	err       error
}

func (f *fakeRehydrator) Rehydrate(_, _ string) (Principal, error) { return f.principal, f.err }

// rule allows or denies the listed actions on resources; a trailing * in a resource
// matches any suffix. An explicit deny wins.
type rule struct {
	deny      bool
	actions   []string
	resources []string
}

func (r rule) matches(action, resource string) bool {
	hit := false
	for _, a := range r.actions {
		hit = hit || a == action
	}
	if !hit {
		return false
	}
	for _, p := range r.resources {
		if p == resource || (strings.HasSuffix(p, "*") && strings.HasPrefix(resource, strings.TrimSuffix(p, "*"))) {
			return true
		}
	}
	return false
}

type fakeAuthorizer struct {
	mu    sync.Mutex
	rules []rule
	err   error
	seen  []string
}

func (f *fakeAuthorizer) Authorize(_ *http.Request, _ Principal, action string, resources []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	for _, res := range resources {
		f.seen = append(f.seen, action+" "+res)
		allowed := false
		for _, r := range f.rules {
			if !r.matches(action, res) {
				continue
			}
			if r.deny {
				return errors.New(awserrors.ErrorAccessDenied)
			}
			allowed = true
		}
		if !allowed {
			return errors.New(awserrors.ErrorAccessDenied)
		}
	}
	return nil
}

type fakeAdmission struct {
	active bool
	err    error
}

func (f fakeAdmission) Active(_, _ string) (bool, error) { return f.active, f.err }

type bridgeEnv struct {
	handler  http.Handler
	issuer   *ecrauth.Issuer
	verifier *ecrauth.Verifier
	rehyd    *fakeRehydrator
	authz    *fakeAuthorizer
	adm      *fakeAdmission
	reached  *int
}

func newBridgeEnv(t *testing.T, rules ...rule) *bridgeEnv {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	master := make([]byte, 32)
	_, err := rand.Read(master)
	require.NoError(t, err)
	key, verify, err := ecrauth.LoadOrCreateSigningKey(t.Context(), js, master)
	require.NoError(t, err)
	const aud = "ecr." + bridgeRegion + ".spinifex.internal"
	iss := ecrauth.NewIssuer(key, aud)

	vf := ecrauth.NewVerifier(verify, aud)
	e := &bridgeEnv{
		issuer: iss, verifier: vf,
		rehyd: &fakeRehydrator{principal: Principal{
			Identity: "dev", AccountID: bridgeAccount, PrincipalType: "user", CallerARN: bridgeUserARN,
		}},
		authz:   &fakeAuthorizer{rules: rules},
		adm:     &fakeAdmission{active: true},
		reached: new(int),
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*e.reached++
		w.WriteHeader(http.StatusOK)
	})
	b, err := NewBridge(BridgeConfig{
		Registry: next, Verifier: vf, Issuer: iss,
		Rehydrator: e.rehyd, Authorizer: e.authz, Admission: e.adm, Region: bridgeRegion, Target: hostTarget, ClientIP: noIP,
	})
	require.NoError(t, err)
	e.handler = hostMiddleware(b.Handler())
	return e
}

func (e *bridgeEnv) token(t *testing.T, account, arn, principalType string) string {
	t.Helper()
	tok, _, err := e.issuer.Mint(ecrauth.Principal{AccountID: account, ARN: arn, Type: principalType, AccessKeyID: bridgeAKID})
	require.NoError(t, err)
	return tok
}

func (e *bridgeEnv) userToken(t *testing.T) string {
	return e.token(t, bridgeAccount, bridgeUserARN, "user")
}

func (e *bridgeEnv) do(method, target string, authz ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	req.Host = bridgeHost
	for _, a := range authz {
		req.Header.Add("Authorization", a)
	}
	w := httptest.NewRecorder()
	e.handler.ServeHTTP(w, req)
	return w
}

func basicAuth(token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("AWS:"+token))
}

func errCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Errors []struct {
			Code string `json:"code"`
		} `json:"errors"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), w.Body.String())
	require.Len(t, body.Errors, 1)
	return body.Errors[0].Code
}

func repoARN(repo string) string {
	return "arn:aws:ecr:" + bridgeRegion + ":" + bridgeAccount + ":repository/" + repo
}

func allow(actions []string, resources ...string) rule {
	return rule{actions: actions, resources: resources}
}

func TestNewBridge_RefusesMissingDependencies(t *testing.T) {
	full := BridgeConfig{
		Verifier: &ecrauth.Verifier{}, Issuer: &ecrauth.Issuer{}, Rehydrator: &fakeRehydrator{},
		Authorizer: &fakeAuthorizer{}, Admission: fakeAdmission{}, Target: noTarget, ClientIP: noIP,
	}
	_, err := NewBridge(full)
	require.NoError(t, err)

	for name, mutate := range map[string]func(*BridgeConfig){
		"verifier":   func(c *BridgeConfig) { c.Verifier = nil },
		"issuer":     func(c *BridgeConfig) { c.Issuer = nil },
		"rehydrator": func(c *BridgeConfig) { c.Rehydrator = nil },
		"authorizer": func(c *BridgeConfig) { c.Authorizer = nil },
		"admission":  func(c *BridgeConfig) { c.Admission = nil },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := full
			mutate(&cfg)
			b, err := NewBridge(cfg)
			require.Error(t, err)
			assert.Nil(t, b)
		})
	}
}

func TestBridge_NilRegistryAnswers501(t *testing.T) {
	e := newBridgeEnv(t, allow([]string{"ecr:GetDownloadUrlForLayer", "ecr:BatchGetImage", "ecr:BatchCheckLayerAvailability"}, repoARN("*")))
	b, err := NewBridge(BridgeConfig{
		Verifier: e.verifier, Issuer: e.issuer, Rehydrator: e.rehyd, Authorizer: e.authz, Admission: e.adm, Region: bridgeRegion, Target: hostTarget, ClientIP: noIP,
	})
	require.NoError(t, err)
	e.handler = hostMiddleware(b.Handler())

	w := e.do(http.MethodGet, "/v2/team/app/manifests/latest", "Bearer "+e.userToken(t))
	assert.Equal(t, http.StatusNotImplemented, w.Code)
	assert.Equal(t, "UNSUPPORTED", errCode(t, w))
}

func TestExtractToken(t *testing.T) {
	good := base64.StdEncoding.EncodeToString([]byte("AWS:abc.def.ghi"))
	cases := []struct {
		name  string
		authz string
		want  string
		ok    bool
	}{
		{"bearer", "Bearer abc.def.ghi", "abc.def.ghi", true},
		{"basic AWS", "Basic " + good, "abc.def.ghi", true},
		{"basic wrong user", "Basic " + base64.StdEncoding.EncodeToString([]byte("root:x")), "", false},
		{"basic empty pass", "Basic " + base64.StdEncoding.EncodeToString([]byte("AWS:")), "", false},
		{"basic bad base64", "Basic !!!notbase64", "", false},
		{"unknown scheme", "AWS4-HMAC-SHA256 cred", "", false},
		{"empty", "", "", false},
		{"bearer empty", "Bearer ", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := extractToken(c.authz)
			assert.Equal(t, c.ok, ok)
			assert.Equal(t, c.want, got)
		})
	}
}

func TestBridge_Authentication(t *testing.T) {
	read := allow([]string{"ecr:BatchGetImage"}, repoARN("*"))

	t.Run("no credential challenges 401", func(t *testing.T) {
		e := newBridgeEnv(t, read)
		w := e.do(http.MethodGet, "/v2/")
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Equal(t, `Bearer realm="https://`+bridgeHost+`/v2/token",service="`+bridgeHost+`"`, w.Header().Get("WWW-Authenticate"))
		assert.Zero(t, *e.reached)
	})
	t.Run("invalid token challenges 401", func(t *testing.T) {
		e := newBridgeEnv(t, read)
		w := e.do(http.MethodGet, "/v2/", "Bearer not.a.token")
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.NotEmpty(t, w.Header().Get("WWW-Authenticate"))
	})
	t.Run("multiple Authorization headers", func(t *testing.T) {
		e := newBridgeEnv(t, read)
		w := e.do(http.MethodGet, "/v2/", "Bearer a.b.c", "Bearer d.e.f")
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
	t.Run("token for another account than the host", func(t *testing.T) {
		e := newBridgeEnv(t, read)
		tok := e.token(t, "000000000099", "arn:aws:iam::000000000099:user/dev", "user")
		w := e.do(http.MethodGet, "/v2/", "Bearer "+tok)
		assert.Equal(t, http.StatusForbidden, w.Code)
		assert.Equal(t, "DENIED", errCode(t, w))
	})
	t.Run("valid token reaches the registry", func(t *testing.T) {
		e := newBridgeEnv(t, read)
		for _, authz := range []string{"Bearer " + e.userToken(t), basicAuth(e.userToken(t))} {
			w := e.do(http.MethodGet, "/v2/team/app/manifests/latest", authz)
			assert.Equal(t, http.StatusOK, w.Code)
		}
		assert.Equal(t, 2, *e.reached)
	})
}

func TestBridge_Rehydration(t *testing.T) {
	read := allow([]string{"ecr:BatchGetImage"}, repoARN("*"))
	get := func(e *bridgeEnv, t *testing.T) *httptest.ResponseRecorder {
		return e.do(http.MethodGet, "/v2/team/app/manifests/latest", "Bearer "+e.userToken(t))
	}

	t.Run("rejected identity challenges 401", func(t *testing.T) {
		e := newBridgeEnv(t, read)
		e.rehyd.err = ErrInvalidCredential
		w := get(e, t)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.NotEmpty(t, w.Header().Get("WWW-Authenticate"))
		assert.Zero(t, *e.reached)
	})
	t.Run("dependency outage is 503", func(t *testing.T) {
		e := newBridgeEnv(t, read)
		e.rehyd.err = ErrUnavailable
		w := get(e, t)
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		assert.Equal(t, "UNKNOWN", errCode(t, w))
		assert.Zero(t, *e.reached)
	})
	t.Run("inactive account stops at once", func(t *testing.T) {
		e := newBridgeEnv(t, read)
		e.adm.active = false
		w := get(e, t)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Zero(t, *e.reached)
	})
	t.Run("unreadable account status is 503", func(t *testing.T) {
		e := newBridgeEnv(t, read)
		e.adm.err = errors.New("nats: timeout")
		w := get(e, t)
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		assert.Zero(t, *e.reached)
	})
	t.Run("principalType claim mismatch", func(t *testing.T) {
		e := newBridgeEnv(t, read)
		e.rehyd.principal.PrincipalType = "assumed-role"
		assert.Equal(t, http.StatusUnauthorized, get(e, t).Code)
	})
	t.Run("subject does not match canonical ARN", func(t *testing.T) {
		e := newBridgeEnv(t, read)
		e.rehyd.principal.CallerARN = "arn:aws:iam::" + bridgeAccount + ":user/other"
		assert.Equal(t, http.StatusUnauthorized, get(e, t).Code)
	})
}

func TestBridge_OperationAuthorization(t *testing.T) {
	account := "arn:aws:ecr:" + bridgeRegion + ":" + bridgeAccount + ":repository/*"
	pullOnly := allow([]string{"ecr:GetDownloadUrlForLayer", "ecr:BatchGetImage", "ecr:BatchCheckLayerAvailability"}, repoARN("team/app"))
	readOnly := allow([]string{
		"ecr:GetDownloadUrlForLayer", "ecr:BatchGetImage", "ecr:BatchCheckLayerAvailability",
		"ecr:ListImages", "ecr:DescribeRepositories",
	}, repoARN("team/app"), account)

	type op struct{ method, path string }
	run := func(t *testing.T, e *bridgeEnv, o op) *httptest.ResponseRecorder {
		return e.do(o.method, o.path, "Bearer "+e.userToken(t))
	}
	matrix := func(t *testing.T, rules []rule, allowed, denied []op) {
		for _, o := range allowed {
			t.Run("allow "+o.method+" "+o.path, func(t *testing.T) {
				e := newBridgeEnv(t, rules...)
				assert.Equal(t, http.StatusOK, run(t, e, o).Code)
				assert.Equal(t, 1, *e.reached)
			})
		}
		for _, o := range denied {
			t.Run("deny "+o.method+" "+o.path, func(t *testing.T) {
				e := newBridgeEnv(t, rules...)
				w := run(t, e, o)
				assert.Equal(t, http.StatusForbidden, w.Code)
				assert.Equal(t, "DENIED", errCode(t, w))
				assert.Zero(t, *e.reached, "denied operation must never dispatch")
			})
		}
	}

	t.Run("pull only", func(t *testing.T) {
		matrix(t, []rule{pullOnly},
			[]op{
				{http.MethodHead, "/v2/team/app/blobs/sha256:abc"},
				{http.MethodGet, "/v2/team/app/blobs/sha256:abc"},
				{http.MethodHead, "/v2/team/app/manifests/latest"},
				{http.MethodGet, "/v2/team/app/manifests/latest"},
				{http.MethodGet, "/v2/team/app/referrers/sha256:abc"},
			},
			[]op{
				{http.MethodPost, "/v2/team/app/blobs/uploads/"},
				{http.MethodPatch, "/v2/team/app/blobs/uploads/upload-1"},
				{http.MethodPut, "/v2/team/app/blobs/uploads/upload-1"},
				{http.MethodDelete, "/v2/team/app/blobs/uploads/upload-1"},
				{http.MethodPut, "/v2/team/app/manifests/latest"},
				{http.MethodDelete, "/v2/team/app/manifests/sha256:abc"},
			})
	})
	t.Run("read only", func(t *testing.T) {
		matrix(t, []rule{readOnly},
			[]op{
				{http.MethodGet, "/v2/_catalog"},
				{http.MethodGet, "/v2/team/app/tags/list"},
				{http.MethodHead, "/v2/team/app/blobs/sha256:abc"},
				{http.MethodGet, "/v2/team/app/manifests/latest"},
				{http.MethodGet, "/v2/team/app/referrers/sha256:abc"},
			},
			[]op{
				{http.MethodPost, "/v2/team/app/blobs/uploads/"},
				{http.MethodPut, "/v2/team/app/manifests/latest"},
				{http.MethodDelete, "/v2/team/app/manifests/sha256:abc"},
			})
	})
	t.Run("an allow scoped to one repository does not cross repositories", func(t *testing.T) {
		matrix(t, []rule{allow([]string{"ecr:BatchGetImage"}, repoARN("team/a"))},
			[]op{{http.MethodGet, "/v2/team/a/manifests/latest"}, {http.MethodGet, "/v2/team/a/referrers/sha256:abc"}},
			[]op{{http.MethodGet, "/v2/team/b/manifests/latest"}, {http.MethodGet, "/v2/team/b/referrers/sha256:abc"}})
	})
	t.Run("referrers need BatchGetImage and nothing a push grants", func(t *testing.T) {
		pushOnly := allow([]string{"ecr:PutImage", "ecr:InitiateLayerUpload", "ecr:ListImages", "ecr:BatchCheckLayerAvailability"}, repoARN("team/app"))
		matrix(t, []rule{pushOnly}, nil, []op{{http.MethodGet, "/v2/team/app/referrers/sha256:abc"}})
	})
	t.Run("explicit deny wins", func(t *testing.T) {
		deny := rule{deny: true, actions: []string{"ecr:BatchGetImage"}, resources: []string{repoARN("team/app")}}
		matrix(t, []rule{allow([]string{"ecr:BatchGetImage"}, account), deny}, nil,
			[]op{{http.MethodGet, "/v2/team/app/manifests/latest"}, {http.MethodGet, "/v2/team/app/referrers/sha256:abc"}})
	})
	t.Run("cross-repository mount needs destination write and source read", func(t *testing.T) {
		mount := op{http.MethodPost, "/v2/team/app/blobs/uploads/?mount=sha256:abc&from=team/base"}
		dest := allow([]string{"ecr:InitiateLayerUpload"}, repoARN("team/app"))
		matrix(t, []rule{dest}, nil, []op{mount})
		matrix(t, []rule{dest, allow([]string{"ecr:BatchCheckLayerAvailability"}, repoARN("team/base"))}, []op{mount}, nil)
	})
	t.Run("an unclassified operation is refused before dispatch", func(t *testing.T) {
		e := newBridgeEnv(t, readOnly)
		w := run(t, e, op{http.MethodPost, "/v2/"})
		assert.Equal(t, http.StatusNotFound, w.Code)
		assert.Zero(t, *e.reached)
	})
	t.Run("the base path needs no action", func(t *testing.T) {
		e := newBridgeEnv(t)
		w := run(t, e, op{http.MethodGet, "/v2/"})
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "registry/2.0", w.Header().Get("Docker-Distribution-Api-Version"))
		assert.Empty(t, e.authz.seen)
	})
	t.Run("an authorization dependency failure is 503 and never dispatches", func(t *testing.T) {
		e := newBridgeEnv(t, readOnly)
		e.authz.err = ErrUnavailable
		w := run(t, e, op{http.MethodGet, "/v2/team/app/manifests/latest"})
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		assert.Zero(t, *e.reached)
	})
}

func TestBridge_TokenEndpoint(t *testing.T) {
	t.Run("exchange mints a fresh token for the same principal", func(t *testing.T) {
		e := newBridgeEnv(t)
		old := e.userToken(t)
		for _, authz := range []string{basicAuth(old), "Bearer " + old} {
			w := e.do(http.MethodGet, "/v2/token", authz)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
			var body ociTokenResponse
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			assert.NotEmpty(t, body.Token)
			assert.Equal(t, body.Token, body.AccessToken)
			assert.Positive(t, body.ExpiresIn)
			assert.NotEmpty(t, body.IssuedAt)
			assert.Equal(t, http.StatusOK, e.do(http.MethodGet, "/v2/", "Bearer "+body.Token).Code)
		}
	})
	t.Run("no credential is a 401 with no challenge", func(t *testing.T) {
		w := newBridgeEnv(t).do(http.MethodGet, "/v2/token")
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Empty(t, w.Header().Get("WWW-Authenticate"))
		assert.Equal(t, "UNAUTHORIZED", errCode(t, w))
	})
	t.Run("invalid credential is a 401 with no challenge", func(t *testing.T) {
		w := newBridgeEnv(t).do(http.MethodGet, "/v2/token", "Bearer not.a.token")
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Empty(t, w.Header().Get("WWW-Authenticate"))
	})
	t.Run("two authorization headers", func(t *testing.T) {
		w := newBridgeEnv(t).do(http.MethodGet, "/v2/token", "Bearer a.b.c", "Bearer d.e.f")
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
	t.Run("token for another account than the host", func(t *testing.T) {
		e := newBridgeEnv(t)
		other := e.token(t, "000000000099", "arn:aws:iam::000000000099:user/dev", "user")
		w := e.do(http.MethodGet, "/v2/token", "Bearer "+other)
		assert.Equal(t, http.StatusForbidden, w.Code)
		assert.Equal(t, "DENIED", errCode(t, w))
	})
	t.Run("a revoked identity is not refreshed", func(t *testing.T) {
		e := newBridgeEnv(t)
		e.rehyd.err = ErrInvalidCredential
		w := e.do(http.MethodGet, "/v2/token", basicAuth(e.userToken(t)))
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Empty(t, w.Header().Get("WWW-Authenticate"))
	})
	t.Run("a dependency outage is a 503", func(t *testing.T) {
		e := newBridgeEnv(t)
		e.rehyd.err = ErrUnavailable
		w := e.do(http.MethodGet, "/v2/token", basicAuth(e.userToken(t)))
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		assert.Equal(t, "UNKNOWN", errCode(t, w))
	})
}
