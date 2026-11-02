package awsapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	"github.com/mulgadc/bluebottle/pkg/sigv4"
	"github.com/mulgadc/spinifex/spinifex/domains/tagging"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	regAccount = "123456789012"
	getTarget  = "ResourceGroupsTaggingAPI_20170126.GetResources"
)

type authCall struct {
	service, action string
	resources       []string
}

// recorder answers every Authorize call with deny and logs what was asked.
type recorder struct {
	auths []authCall
	deny  error
}

func (f *recorder) invocation(target, body, account string) dispatch.Invocation {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	if target != "" {
		req.Header.Set("X-Amz-Target", target)
	}
	return dispatch.Invocation{
		Request:   req,
		AccountID: account,
		Authorize: func(service, action string, resources []string, _ iampolicy.ConditionKeys) error {
			f.auths = append(f.auths, authCall{service, action, resources})
			return f.deny
		},
	}
}

func dispatchWith(deps Deps, inv dispatch.Invocation) (*httptest.ResponseRecorder, error) {
	w := httptest.NewRecorder()
	return w, NewRegistration(deps).Dispatch(w, inv)
}

// An action is added to Actions only with a scope-table entry, and a scope
// entry is removed with its action, so an action cannot ship with a silent
// account-wide grant.
func TestActionsMatchScopeTable(t *testing.T) {
	for action := range Actions {
		assert.True(t, tagging.HasScope(action),
			"tagging action %q has no resource scope entry: add one to taggingScopes in domains/tagging/authz.go", action)
	}
	for _, action := range tagging.ScopedActions() {
		_, ok := Actions[action]
		assert.True(t, ok,
			"taggingScopes has an entry for %q, which the dispatch table does not serve: remove it from domains/tagging/authz.go", action)
	}
}

func TestOperationInventory_AllImplemented(t *testing.T) {
	inv := OperationInventory()
	assert.Equal(t, []string{"GetResources"}, inv.Registered)
	assert.Empty(t, inv.Stubbed)
	assert.Empty(t, inv.Unsupported)
}

func TestRegistration_Identity(t *testing.T) {
	reg := NewRegistration(Deps{})
	assert.Equal(t, "tagging", reg.Service)
	assert.Equal(t, dispatch.ErrorEnvelopeJSON, reg.Errors)
}

func TestResolveAction_Forms(t *testing.T) {
	cases := map[string]string{
		getTarget:                     "GetResources",
		"GetResources":                "GetResources",
		"a.b.GetResources":            "GetResources",
		"ResourceGroupsTaggingAPI_1.": "",
		"":                            "",
	}
	for target, want := range cases {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		if target != "" {
			req.Header.Set("X-Amz-Target", target)
		}
		assert.Equal(t, want, NewRegistration(Deps{}).ResolveAction(req), target)
	}
}

func TestDispatch_MissingTargetAndUnknownAction(t *testing.T) {
	rec := &recorder{}
	_, err := dispatchWith(Deps{}, rec.invocation("", "{}", regAccount))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingAction, err.Error())

	_, err = dispatchWith(Deps{}, rec.invocation("ResourceGroupsTaggingAPI_20170126.TagResources", "{}", regAccount))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAction, err.Error())
	assert.Empty(t, rec.auths, "an unrouted action is rejected before authorization")
}

// Every action is account-level: the policy check sees tagging:<Action> on "*".
func TestDispatch_AuthorizesAccountLevel(t *testing.T) {
	for _, action := range tagging.ScopedActions() {
		rec := &recorder{}
		_, _ = dispatchWith(Deps{Aggregator: tagging.NewAggregator()}, rec.invocation("X."+action, "{}", regAccount))
		require.Len(t, rec.auths, 1, action)
		assert.Equal(t, authCall{"tagging", action, []string{"*"}}, rec.auths[0])
	}
}

func TestDispatch_DenyStopsBeforeBodyAndAggregator(t *testing.T) {
	rec := &recorder{deny: errors.New(awserrors.ErrorAccessDenied)}
	// The aggregator is nil and the body oversized: only the denial may surface.
	body := strings.Repeat("a", int(sigv4.MaxPayloadLen)+1)
	_, err := dispatchWith(Deps{}, rec.invocation(getTarget, body, regAccount))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
}

func TestDispatch_NoAccountIsServerInternal(t *testing.T) {
	rec := &recorder{}
	_, err := dispatchWith(Deps{Aggregator: tagging.NewAggregator()}, rec.invocation(getTarget, "{}", ""))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
	assert.Len(t, rec.auths, 1, "authorization runs before the account check")
}

func TestDispatch_OversizedBodyRejectedAfterAuthorization(t *testing.T) {
	rec := &recorder{}
	body := `{"PaginationToken":"` + strings.Repeat("a", int(sigv4.MaxPayloadLen)) + `"}`
	_, err := dispatchWith(Deps{Aggregator: tagging.NewAggregator()}, rec.invocation(getTarget, body, regAccount))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorRequestEntityTooLarge, err.Error())
	assert.Len(t, rec.auths, 1)
}

func TestDispatch_NoAggregatorIsServerInternal(t *testing.T) {
	rec := &recorder{}
	_, err := dispatchWith(Deps{}, rec.invocation(getTarget, "{}", regAccount))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestDispatch_WiredAggregatorServesGetResources(t *testing.T) {
	rec := &recorder{}
	w, err := dispatchWith(Deps{Aggregator: tagging.NewAggregator()}, rec.invocation(getTarget, "{}", regAccount))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, tagging.JSONContentType, w.Header().Get("Content-Type"))
	assert.JSONEq(t, `{"PaginationToken":""}`, w.Body.String())
}
