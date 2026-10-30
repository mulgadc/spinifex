package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/ochre"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const accessTestModelID = "meta.llama3-2-1b-instruct-v1:0"

// authzCall records what the dispatcher asked ingress to authorize.
type authzCall struct{ service, action string }

// serve runs one Query request through the dispatcher with allow-all (or the
// given) authorization and returns the recorder and the dispatch error.
func serve(t *testing.T, d Deps, accountID string, args url.Values, authorize dispatch.AuthorizeFunc) (*httptest.ResponseRecorder, error) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(args.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if authorize == nil {
		authorize = func(string, string, []string, iampolicy.ConditionKeys) error { return nil }
	}
	w := httptest.NewRecorder()
	err := d.dispatch(w, dispatch.Invocation{Request: req, AccountID: accountID, Authorize: authorize})
	return w, err
}

func action(name string, kv ...string) url.Values {
	v := url.Values{"Action": {name}}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v
}

func requireCode(t *testing.T, err error, want string) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, want, err.Error())
}

func TestRegistration_GetVersion_Admin(t *testing.T) {
	w, err := serve(t, Deps{Version: "v0.5.0-43-gae1deb5", Commit: "ae1deb5"}, awsidentifiers.DefaultAccountID(), action("GetVersion"), nil)

	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	assert.Contains(t, w.Body.String(), `"version":"v0.5.0-43-gae1deb5"`)
	assert.Contains(t, w.Body.String(), `"license":"open-source"`)
}

func TestRegistration_EveryActionDeniesNonAdminAccount(t *testing.T) {
	for _, name := range ActionNames() {
		t.Run(name, func(t *testing.T) {
			_, err := serve(t, Deps{}, "000000000002", action(name), nil)
			requireCode(t, err, awserrors.ErrorAccessDenied)
		})
	}
}

// Authorization is asked first, with the wildcard resource, so a caller the
// policy refuses is told so before the account or action is looked at.
func TestRegistration_PolicyDenialPrecedesAccountAndActionChecks(t *testing.T) {
	var calls []authzCall
	deny := func(service, act string, resources []string, _ iampolicy.ConditionKeys) error {
		calls = append(calls, authzCall{service, act})
		assert.Equal(t, []string{"*"}, resources)
		return errors.New(awserrors.ErrorAccessDenied)
	}

	for _, name := range []string{"GetVersion", "NotAnAction"} {
		_, err := serve(t, Deps{}, "", action(name), deny)
		requireCode(t, err, awserrors.ErrorAccessDenied)
	}
	assert.Equal(t, []authzCall{{"spinifex", "GetVersion"}, {"spinifex", "NotAnAction"}}, calls)
}

func TestRegistration_UnknownAndMissingAction(t *testing.T) {
	_, err := serve(t, Deps{}, awsidentifiers.DefaultAccountID(), action("DoesNotExist"), nil)
	requireCode(t, err, awserrors.ErrorInvalidAction)

	// Unknown actions are not account-gated: a tenant gets the same answer.
	_, err = serve(t, Deps{}, "000000000002", action("DoesNotExist"), nil)
	requireCode(t, err, awserrors.ErrorInvalidAction)

	_, err = serve(t, Deps{}, awsidentifiers.DefaultAccountID(), url.Values{"Version": {"1"}}, nil)
	requireCode(t, err, awserrors.ErrorMissingAction)
}

func TestRegistration_MalformedQueryString(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("Action=GetVersion&Bad=%ZZ"))
	err := Deps{}.dispatch(httptest.NewRecorder(), dispatch.Invocation{Request: req, AccountID: awsidentifiers.DefaultAccountID()})
	requireCode(t, err, awserrors.ErrorMalformedQueryString)
}

func TestRegistration_NoAccountIDIsServerInternal(t *testing.T) {
	_, err := serve(t, Deps{}, "", action("GetVersion"), nil)
	requireCode(t, err, awserrors.ErrorServerInternal)
}

func TestRegistration_NATSActionsWithoutConnectionAreServerInternal(t *testing.T) {
	for _, name := range []string{"GetNodes", "GetVMs", "GetStorageStatus"} {
		t.Run(name, func(t *testing.T) {
			_, err := serve(t, Deps{}, awsidentifiers.DefaultAccountID(), action(name), nil)
			requireCode(t, err, awserrors.ErrorServerInternal)
		})
	}

	_, err := serve(t, Deps{}, awsidentifiers.DefaultAccountID(), action("PromoteImage", "ImageId", "ami-1"), nil)
	requireCode(t, err, awserrors.ErrorServerInternal)
}

func TestRegistration_PromoteImageRequiresImageID(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	_, err := serve(t, Deps{NATS: nc}, awsidentifiers.DefaultAccountID(), action("PromoteImage"), nil)
	requireCode(t, err, awserrors.ErrorMissingParameter)
}

func TestRegistration_ListOchreCatalogNeedsNoStore(t *testing.T) {
	w, err := serve(t, Deps{}, awsidentifiers.DefaultAccountID(), action("ListOchreCatalog"), nil)

	require.NoError(t, err)
	var out ochre.AdminCatalogList
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.NotEmpty(t, out.Entries)
}

// With nothing wired, the catalog resolves no credentials and grants no
// access, so no entry is available to the account.
func TestRegistration_DefaultResolversGrantNothing(t *testing.T) {
	d := Deps{}
	assert.Equal(t, ochre.NoopCredentialResolver, d.credentials())

	granted, err := d.access().Granted(context.Background(), "000000000002", accessTestModelID)
	require.NoError(t, err)
	assert.False(t, granted)

	d.Access = grantAll{}
	granted, err = d.access().Granted(context.Background(), "000000000002", accessTestModelID)
	require.NoError(t, err)
	assert.True(t, granted)
}

type grantAll struct{}

func (grantAll) Granted(context.Context, string, string) (bool, error) { return true, nil }

func TestRegistration_ActiveNodesIsOptional(t *testing.T) {
	assert.Zero(t, Deps{}.activeNodes(context.Background()))
	assert.Equal(t, 3, Deps{ActiveNodes: func(context.Context) int { return 3 }}.activeNodes(context.Background()))
}

func accessDeps(t *testing.T) Deps {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	return Deps{ModelAccess: ochre.NewModelAccessStore(js)}
}

// The operator's whole loop: an account starts with nothing, a grant shows up
// in the listing, and a revoke takes it away again.
func TestRegistration_ModelAccess_GrantListRevoke(t *testing.T) {
	d := accessDeps(t)
	adminAccount := awsidentifiers.DefaultAccountID()
	target := "000000000002"

	w, err := serve(t, d, adminAccount, action("ListModelAccess", "AccountId", target), nil)
	require.NoError(t, err)
	assert.NotContains(t, w.Body.String(), accessTestModelID)

	w, err = serve(t, d, adminAccount, action("GrantModelAccess", "AccountId", target, "ModelId", accessTestModelID), nil)
	require.NoError(t, err)
	assert.Contains(t, w.Body.String(), accessTestModelID)

	w, err = serve(t, d, adminAccount, action("ListModelAccess", "AccountId", target), nil)
	require.NoError(t, err)
	assert.Contains(t, w.Body.String(), accessTestModelID)

	_, err = serve(t, d, adminAccount, action("RevokeModelAccess", "AccountId", target, "ModelId", accessTestModelID), nil)
	require.NoError(t, err)

	w, err = serve(t, d, adminAccount, action("ListModelAccess", "AccountId", target), nil)
	require.NoError(t, err)
	assert.NotContains(t, w.Body.String(), accessTestModelID)
}

func TestRegistration_ModelAccess_MissingParameters(t *testing.T) {
	d := accessDeps(t)
	cases := []url.Values{
		action("GrantModelAccess", "ModelId", accessTestModelID),
		action("GrantModelAccess", "AccountId", "000000000002"),
		action("RevokeModelAccess", "ModelId", accessTestModelID),
		action("ListModelAccess"),
	}
	for _, args := range cases {
		_, err := serve(t, d, awsidentifiers.DefaultAccountID(), args, nil)
		requireCode(t, err, awserrors.ErrorMissingParameter)
	}
}

func TestRegistration_ModelAccess_NoStoreIsServerInternal(t *testing.T) {
	for _, name := range []string{"GrantModelAccess", "RevokeModelAccess", "ListModelAccess"} {
		_, err := serve(t, Deps{}, awsidentifiers.DefaultAccountID(),
			action(name, "AccountId", "000000000002", "ModelId", accessTestModelID), nil)
		requireCode(t, err, awserrors.ErrorServerInternal)
	}
}

type failingAccess struct{}

func (failingAccess) Grant(context.Context, string, string) error  { return errors.New("kv down") }
func (failingAccess) Revoke(context.Context, string, string) error { return errors.New("kv down") }
func (failingAccess) List(context.Context, string) ([]string, error) {
	return nil, errors.New("kv down")
}

func TestRegistration_ModelAccess_StoreErrorsPropagate(t *testing.T) {
	d := Deps{ModelAccess: failingAccess{}}
	for _, name := range []string{"GrantModelAccess", "RevokeModelAccess", "ListModelAccess"} {
		w, err := serve(t, d, awsidentifiers.DefaultAccountID(),
			action(name, "AccountId", "000000000002", "ModelId", accessTestModelID), nil)
		requireCode(t, err, "kv down")
		assert.Empty(t, w.Body.String(), "a failed action writes no success body")
	}
}

func TestNewRegistration_DeclaresTheAdminActionsAsItsInventory(t *testing.T) {
	reg := NewRegistration(Deps{})

	assert.Equal(t, ServiceName, reg.Service)
	assert.Equal(t, dispatch.ErrorEnvelopeXML, reg.Errors)
	assert.ElementsMatch(t, []string{
		"GetVersion", "GetNodes", "GetVMs", "GetStorageStatus", "PromoteImage",
		"GrantModelAccess", "RevokeModelAccess", "ListModelAccess", "ListOchreCatalog",
		"EnsureSQSStore",
		"EnsureSQSWorker",
	}, reg.Inventory.Registered)
	assert.NoError(t, dispatch.NewBuilder().Register(reg))
}
