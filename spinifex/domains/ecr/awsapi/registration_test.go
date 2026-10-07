package awsapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	"github.com/mulgadc/bluebottle/pkg/sigv4"
	handlers_ecr "github.com/mulgadc/spinifex/spinifex/domains/ecr"
	ecrauth "github.com/mulgadc/spinifex/spinifex/domains/ecr/auth"
	ecrregistry "github.com/mulgadc/spinifex/spinifex/domains/ecr/registry"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	regAccount = "123456789012"
	regRegion  = "us-east-1"
)

var errFakeBackend = errors.New("fake backend")

// backend fakes every composed capability. Each call is appended to log, so a
// test can prove what ran, in what order, and for which repository.
type backend struct {
	log      *[]string
	repos    []string
	minted   []ecrauth.Principal
	mintFail bool
}

func (b *backend) use(repo string) {
	*b.log = append(*b.log, "use")
	b.repos = append(b.repos, repo)
}

func (b *backend) ListImages(_ context.Context, _, repo string) ([]ecrregistry.ImageRecord, error) {
	b.use(repo)
	return nil, errFakeBackend
}

func (b *backend) GetManifest(_ context.Context, _, repo, _ string, _ []string) ([]byte, string, string, error) {
	b.use(repo)
	return nil, "", "", errFakeBackend
}

func (b *backend) StoreManifest(_ context.Context, _, repo, _, _ string, _ []byte) (string, error) {
	b.use(repo)
	return "", errFakeBackend
}

func (b *backend) DeleteImage(_ context.Context, _, repo, _, _ string) (string, error) {
	b.use(repo)
	return "", errFakeBackend
}

func (b *backend) GetLifecyclePolicy(_ context.Context, _, repo string) ([]byte, error) {
	b.use(repo)
	return nil, errFakeBackend
}

func (b *backend) GetRepo(_ context.Context, _, repo string) (handlers_ecr.RepoMeta, error) {
	b.use(repo)
	return handlers_ecr.RepoMeta{}, errFakeBackend
}

func (b *backend) ListRepos(context.Context, string) ([]string, error) {
	b.use("")
	return nil, errFakeBackend
}

func (b *backend) PutRepo(_ context.Context, _ string, meta handlers_ecr.RepoMeta) error {
	b.use(meta.Name)
	return errFakeBackend
}

func (b *backend) ListManifests(_ context.Context, _, repo string) ([]string, error) {
	b.use(repo)
	return nil, errFakeBackend
}

func (b *backend) DeleteRepo(_ context.Context, _, repo string) error {
	b.use(repo)
	return errFakeBackend
}

func (b *backend) Mint(p ecrauth.Principal) (string, time.Time, error) {
	b.use("")
	b.minted = append(b.minted, p)
	if b.mintFail {
		return "", time.Time{}, errFakeBackend
	}
	return "jwt", time.Now().Add(time.Hour), nil
}

var (
	_ ImageCatalog             = (*backend)(nil)
	_ ManifestReader           = (*backend)(nil)
	_ ManifestWriter           = (*backend)(nil)
	_ ImageDeleter             = (*backend)(nil)
	_ LifecyclePolicyStore     = (*backend)(nil)
	_ RepositoryStore          = (*backend)(nil)
	_ AuthorizationTokenIssuer = (*backend)(nil)
)

func (b *backend) deps() Deps {
	endpoint := RepositoryEndpoint{Region: regRegion, ServicesDomain: "example.test"}
	return Deps{
		Registry:           NewRegistryActionService(b, b, b, b),
		LifecyclePreview:   NewLifecyclePreviewActionService(b, b),
		Repository:         NewRepositoryActionService(b, endpoint),
		AuthorizationToken: NewAuthorizationTokenActionService(b, endpoint),
	}
}

// authCall is one inv.Authorize call as the dispatcher made it.
type authCall struct {
	service, action string
	resources       []string
	keys            iampolicy.ConditionKeys
}

// fakeInvocation records Authorize and Actor calls into log alongside the
// backend's, and answers them with deny and actor/actorErr.
type fakeInvocation struct {
	log      []string
	auths    []authCall
	deny     error
	actor    dispatch.Actor
	actorErr error
}

func (f *fakeInvocation) invocation(target, body string) dispatch.Invocation {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	if target != "" {
		req.Header.Set("X-Amz-Target", target)
	}
	return dispatch.Invocation{
		Request:   req,
		AccountID: regAccount,
		Region:    regRegion,
		Authorize: func(service, action string, resources []string, keys iampolicy.ConditionKeys) error {
			f.log = append(f.log, "authorize")
			f.auths = append(f.auths, authCall{service, action, resources, keys})
			return f.deny
		},
		Actor: func() (dispatch.Actor, error) {
			f.log = append(f.log, "actor")
			return f.actor, f.actorErr
		},
	}
}

func dispatchWith(t *testing.T, deps Deps, inv dispatch.Invocation) (*httptest.ResponseRecorder, error) {
	t.Helper()
	w := httptest.NewRecorder()
	return w, NewRegistration(deps).Dispatch(w, inv)
}

// composedCases is one action per composed capability, each naming repository
// "app" in its body, so its scope and its use case can be compared.
var composedCases = []struct{ action, body string }{
	{"ListImages", `{"repositoryName":"app"}`},
	{"GetLifecyclePolicyPreview", `{"repositoryName":"app"}`},
	{"DeleteRepository", `{"repositoryName":"app"}`},
}

func TestActionFromTarget(t *testing.T) {
	for target, want := range map[string]string{
		TargetPrefix + ".CreateRepository": "CreateRepository",
		"a.b.ListRepositories":             "ListRepositories",
		"GetAuthorizationToken":            "GetAuthorizationToken",
		TargetPrefix + ".":                 "",
		"":                                 "",
	} {
		assert.Equal(t, want, actionFromTarget(target), "target %q", target)
	}
}

// Every composed action must be in the Actions namespace, or dispatch rejects
// it as InvalidAction before its resources are ever scoped.
func TestComposedActionsAreInTheNamespace(t *testing.T) {
	for _, names := range [][]string{
		RegistryActionNames(), LifecyclePreviewActionNames(),
		RepositoryActionNames(), AuthorizationTokenActionNames(),
	} {
		for _, action := range names {
			_, ok := Actions[action]
			assert.True(t, ok, "composed ECR action %q is not in Actions, so dispatch rejects it as InvalidAction", action)
		}
	}
}

// The coverage tool reads OperationInventory directly; a live registration
// must declare exactly the same inventory to the registry.
func TestRegistrationInventoryIsTheDeclaredInventory(t *testing.T) {
	log := []string{}
	reg := NewRegistration((&backend{log: &log}).deps())
	assert.Equal(t, OperationInventory(), reg.Inventory)

	b := dispatch.NewBuilder()
	require.NoError(t, b.Register(reg))
	assert.Equal(t, map[string]dispatch.Inventory{ServiceName: OperationInventory()}, b.Build().Inventory())
}

func TestRegistrationDeclaresJSONEnvelopeForECR(t *testing.T) {
	reg := NewRegistration(Deps{})
	assert.Equal(t, ServiceName, reg.Service)
	assert.Equal(t, dispatch.ErrorEnvelopeJSON, reg.Errors)

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Amz-Target", TargetPrefix+".ListImages")
	assert.Equal(t, "ListImages", reg.ResolveAction(req))
}

// A denied request must not reach any composed use case, and a permitted one
// reaches it only after Authorize, for the repository Authorize was scoped to.
func TestDispatch_AuthorizesBeforeEveryUseCase(t *testing.T) {
	for _, tc := range composedCases {
		t.Run(tc.action+"/denied", func(t *testing.T) {
			f := &fakeInvocation{deny: errors.New(awserrors.ErrorAccessDenied)}
			be := &backend{log: &f.log}
			_, err := dispatchWith(t, be.deps(), f.invocation(TargetPrefix+"."+tc.action, tc.body))
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
			assert.Equal(t, []string{"authorize"}, f.log)
		})
		t.Run(tc.action+"/permitted", func(t *testing.T) {
			f := &fakeInvocation{}
			be := &backend{log: &f.log}
			_, _ = dispatchWith(t, be.deps(), f.invocation(TargetPrefix+"."+tc.action, tc.body))
			require.GreaterOrEqual(t, len(f.log), 2)
			assert.Equal(t, "authorize", f.log[0])
			assert.NotContains(t, f.log[1:], "authorize")
			require.Len(t, f.auths, 1)
			assert.Equal(t, authCall{
				service:   ServiceName,
				action:    tc.action,
				resources: []string{"arn:aws:ecr:" + regRegion + ":" + regAccount + ":repository/app"},
			}, f.auths[0])
			assert.Equal(t, []string{"app"}, be.repos, "the use case must act on the repository that was authorized")
		})
	}

	t.Run("GetAuthorizationToken/denied", func(t *testing.T) {
		f := &fakeInvocation{deny: errors.New(awserrors.ErrorAccessDenied)}
		be := &backend{log: &f.log}
		_, err := dispatchWith(t, be.deps(), f.invocation(TargetPrefix+".GetAuthorizationToken", "{}"))
		require.Error(t, err)
		assert.Equal(t, []string{"authorize"}, f.log, "neither the actor nor the issuer may run once denied")
	})
}

// Requests dispatch rejects before scoping never reach Authorize.
func TestDispatch_RejectsBeforeAuthorize(t *testing.T) {
	cases := []struct {
		name, target, body string
		account            string
		want               string
	}{
		{"absent target", "", "{}", regAccount, awserrors.ErrorMissingAction},
		{"trailing dot", TargetPrefix + ".", "{}", regAccount, awserrors.ErrorMissingAction},
		{"unknown action", TargetPrefix + ".MadeUpAction", "{}", regAccount, awserrors.ErrorInvalidAction},
		{"other service's action", "AmazonEC2ContainerServiceV20141113.ListClusters", "{}", regAccount, awserrors.ErrorInvalidAction},
		{"missing account", TargetPrefix + ".ListRepositories", "{}", "", awserrors.ErrorInternalError},
		{"oversized body", TargetPrefix + ".ListImages", strings.Repeat("x", sigv4.MaxPayloadLen+1), regAccount, awserrors.ErrorRequestEntityTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeInvocation{}
			inv := f.invocation(tc.target, tc.body)
			inv.AccountID = tc.account
			_, err := dispatchWith(t, Deps{}, inv)
			require.Error(t, err)
			assert.Equal(t, tc.want, err.Error())
			assert.Empty(t, f.auths)
		})
	}
}

// An unwired capability answers ServerInternal once authorized; a stub with
// no capability still answers its own NotImplemented.
func TestDispatch_NilCapabilityIsServerInternal(t *testing.T) {
	for _, action := range []string{"ListImages", "GetLifecyclePolicyPreview", "DeleteRepository", "GetAuthorizationToken"} {
		t.Run(action, func(t *testing.T) {
			f := &fakeInvocation{}
			_, err := dispatchWith(t, Deps{}, f.invocation(TargetPrefix+"."+action, `{"repositoryName":"app"}`))
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
			assert.Equal(t, []string{"authorize"}, f.log)
		})
	}

	f := &fakeInvocation{}
	_, err := dispatchWith(t, Deps{}, f.invocation(TargetPrefix+".ListRepositories", "{}"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorNotImplemented, err.Error())
}

// The token is minted for the verified actor exactly, whatever identity the
// body claims, and answers in the AWS JSON 1.1 content type.
func TestDispatch_GetAuthorizationTokenMintsForTheVerifiedActor(t *testing.T) {
	actor := dispatch.Actor{
		AccountID:     regAccount,
		CallerARN:     "arn:aws:sts::" + regAccount + ":assumed-role/builder/session",
		PrincipalType: "AssumedRole",
		AccessKeyID:   "ASIAREGISTRATIONTEST",
	}
	f := &fakeInvocation{actor: actor}
	be := &backend{log: &f.log}
	w, err := dispatchWith(t, be.deps(), f.invocation(TargetPrefix+".GetAuthorizationToken",
		`{"registryIds":["999999999999"],"AccountID":"999999999999","ARN":"arn:aws:iam::999999999999:root"}`))
	require.NoError(t, err)

	assert.Equal(t, []string{"authorize", "actor", "use"}, f.log)
	require.Len(t, be.minted, 1)
	assert.Equal(t, ecrauth.Principal{
		AccountID:   actor.AccountID,
		ARN:         actor.CallerARN,
		Type:        actor.PrincipalType,
		AccessKeyID: actor.AccessKeyID,
	}, be.minted[0])
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, JSONContentType, w.Header().Get("Content-Type"))
}

// An actor ingress cannot build is a server fault, and nothing is minted.
func TestDispatch_GetAuthorizationTokenActorFailureIsServerInternal(t *testing.T) {
	f := &fakeInvocation{actorErr: errors.New("no canonical ARN")}
	be := &backend{log: &f.log}
	_, err := dispatchWith(t, be.deps(), f.invocation(TargetPrefix+".GetAuthorizationToken", "{}"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
	assert.Empty(t, be.minted)
}
