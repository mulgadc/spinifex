package admin

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	operatorv1 "github.com/mulgadc/spinifex/contracts/operator/v1"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPathMethod(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		want   string
		wantOK bool
	}{
		{"known method", "/admin/CreateAccount", "CreateAccount", true},
		{"deletion method", "/admin/DeleteAccount", "DeleteAccount", true},
		{"unknown method", "/admin/PurgeEverything", "", false},
		{"nested path", "/admin/CreateAccount/extra", "", false},
		{"bare prefix", "/admin/", "", false},
		{"not admin", "/", "", false},
		{"case mismatch", "/admin/createaccount", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := PathMethod(tc.path)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

// The credential-minting CLI grants these by name. A method served but absent
// from the list would be uncallable by every key an operator issues.
func TestPathMethod_AcceptsExactlyTheContractList(t *testing.T) {
	names := operatorv1.AdminMethodNames()

	require.Len(t, names, len(methods))
	assert.True(t, sort.StringsAreSorted(names), "names must be sorted for stable help text")
	for _, name := range names {
		got, ok := PathMethod(pathPrefix + name)
		require.True(t, ok, "%s is granted but not routed", name)
		assert.Equal(t, name, got)
	}
}

// The registration labels a request with the method its path names, so the
// throttle bucket and trace span are per-method rather than one shared bucket.
func TestResolveAction_LabelsAdminPathByMethod(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/admin/ListAccounts", strings.NewReader("{}"))

	assert.Equal(t, "ListAccounts", resolveAction(req))
}

func adminActor() dispatch.Actor {
	return dispatch.Actor{
		AccountID:     awsidentifiers.DefaultAccountID(),
		PrincipalType: "user",
		UserName:      "signup",
	}
}

func allowAll(_, _ string, _ []string, _ iampolicy.ConditionKeys) error { return nil }

func denyAll(_, _ string, _ []string, _ iampolicy.ConditionKeys) error {
	return awserrors.Errorf(awserrors.ErrorAccessDenied, "denied")
}

// Every gate must deny on its own, and every denial must be indistinguishable
// from the others so a caller cannot learn which one it failed.
func TestAuthorizeREST_Gates(t *testing.T) {
	with := func(mod func(*dispatch.Actor)) dispatch.Actor {
		a := adminActor()
		mod(&a)
		return a
	}
	tests := []struct {
		name      string
		service   string
		actor     dispatch.Actor
		authorize dispatch.AuthorizeFunc
		wantErr   string
	}{
		{"permitted signup principal", ServiceName, adminActor(), allowAll, ""},
		{"credential scope is not spinifex", "ec2", adminActor(), allowAll, awserrors.ErrorAccessDenied},
		{"not the super-admin account", ServiceName, with(func(a *dispatch.Actor) { a.AccountID = "123456789012" }), allowAll, awserrors.ErrorAccessDenied},
		{"assumed-role session", ServiceName, with(func(a *dispatch.Actor) {
			a.PrincipalType = "assumed-role"
			a.SessionName = "signup"
		}), allowAll, awserrors.ErrorAccessDenied},
		{"no identity", ServiceName, with(func(a *dispatch.Actor) { a.UserName = "" }), allowAll, awserrors.ErrorAccessDenied},
		{"policy does not grant the method", ServiceName, adminActor(), denyAll, awserrors.ErrorAccessDenied},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/admin/CreateAccount", nil)
			inv := dispatch.Invocation{Request: req, Service: tc.service, Authorize: tc.authorize}

			err := Deps{}.authorizeREST(inv, "CreateAccount", tc.actor)

			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			code, _ := awserrors.ResolveErrorCode(err)
			assert.Equal(t, tc.wantErr, code)
		})
	}
}

// The policy check is asked for the spinifex service, the method as the
// action and the account-wide resource.
func TestAuthorizeREST_AsksPolicyForTheMethod(t *testing.T) {
	var service, action string
	var resources []string
	authorize := func(s, a string, r []string, _ iampolicy.ConditionKeys) error {
		service, action, resources = s, a, r
		return nil
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/DeleteAccount", nil)
	inv := dispatch.Invocation{Request: req, Service: ServiceName, Authorize: authorize}

	require.NoError(t, Deps{}.authorizeREST(inv, "DeleteAccount", adminActor()))

	assert.Equal(t, ServiceName, service)
	assert.Equal(t, "DeleteAccount", action)
	assert.Equal(t, []string{"*"}, resources)
}

func restInvocation(method, path, body string, authorize dispatch.AuthorizeFunc, service string) (dispatch.Invocation, *httptest.ResponseRecorder) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	return dispatch.Invocation{
		Request:   req,
		Service:   service,
		Authorize: authorize,
		Actor:     func() (dispatch.Actor, error) { return adminActor(), nil },
	}, httptest.NewRecorder()
}

// The verb and the method are checked before authorization, so a probe learns
// nothing about the caller's permissions from either rejection.
func TestDispatchREST_VerbAndMethodBeforeAuthorization(t *testing.T) {
	var authorized bool
	authorize := func(_, _ string, _ []string, _ iampolicy.ConditionKeys) error {
		authorized = true
		return nil
	}

	inv, w := restInvocation(http.MethodGet, "/admin/CreateAccount", "", authorize, "ec2")
	err := Deps{}.dispatchREST(w, inv)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMethodNotAllowed, err.Error())

	inv, w = restInvocation(http.MethodPost, "/admin/PurgeEverything", "{}", authorize, "ec2")
	err = Deps{}.dispatchREST(w, inv)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAction, err.Error())

	assert.False(t, authorized, "authorization must not run before the verb and method checks")
}

// A denied caller learns nothing about whether the cluster is up: the policy
// check runs before the NATS check.
func TestDispatchREST_AuthorizationBeforeClusterCheck(t *testing.T) {
	inv, w := restInvocation(http.MethodPost, "/admin/CreateAccount", "{}", denyAll, ServiceName)

	err := Deps{}.dispatchREST(w, inv)

	require.Error(t, err)
	code, _ := awserrors.ResolveErrorCode(err)
	assert.Equal(t, awserrors.ErrorAccessDenied, code)
}

func TestDispatchREST_AuthorizedWithoutClusterIsUnavailable(t *testing.T) {
	inv, w := restInvocation(http.MethodPost, "/admin/CreateAccount", "{}", allowAll, ServiceName)

	err := Deps{}.dispatchREST(w, inv)

	require.Error(t, err)
	code, message, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorServiceUnavailable, code)
	assert.Equal(t, clusterUnavailableMsg, message)
	assert.Zero(t, w.Body.Len(), "an error must not also write a success body")
}
