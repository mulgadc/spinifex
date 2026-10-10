package awsapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	"github.com/mulgadc/bluebottle/pkg/sigv4"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	regAccount = "123456789012"
	regRegion  = "us-east-1"
)

// authCall is one inv.Authorize call as the dispatcher made it.
type authCall struct {
	service, action string
	resources       []string
	keys            iampolicy.ConditionKeys
}

// fakeInvocation records every Authorize call into log, answering each with
// deny.
type fakeInvocation struct {
	log   []string
	auths []authCall
	deny  error
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
	}
}

func dispatchWith(t *testing.T, deps Deps, inv dispatch.Invocation) (*httptest.ResponseRecorder, error) {
	t.Helper()
	w := httptest.NewRecorder()
	return w, NewRegistration(deps).Dispatch(w, inv)
}

func TestActionFromTarget(t *testing.T) {
	for target, want := range map[string]string{
		"CertificateManager.ListCertificates": "ListCertificates",
		"a.b.ListCertificates":                "ListCertificates",
		"ListCertificates":                    "ListCertificates",
		"CertificateManager.":                 "",
		"":                                    "",
	} {
		assert.Equal(t, want, actionFromTarget(target), "target %q", target)
	}
}

// TestActionsScopeTableIsExhaustive is what stops the next ACM action being
// added with a silent account-wide grant. It asserts both directions, so a
// scope left behind by a deleted or renamed action fails too.
func TestActionsScopeTableIsExhaustive(t *testing.T) {
	for action := range Actions {
		assert.True(t, HasScope(action),
			"ACM action %q has no resource scope entry: add one to authz.go", action)
	}
	for _, action := range ScopedActions() {
		_, ok := Actions[action]
		assert.True(t, ok,
			"acmScopes has an entry for %q, which Actions does not serve: remove it from authz.go", action)
	}
}

// The coverage tool reads OperationInventory directly; a live registration
// must declare exactly the same inventory to the registry.
func TestRegistrationInventoryIsTheDeclaredInventory(t *testing.T) {
	reg := NewRegistration(Deps{})
	assert.Equal(t, OperationInventory(), reg.Inventory)

	b := dispatch.NewBuilder()
	require.NoError(t, b.Register(reg))
	assert.Equal(t, map[string]dispatch.Inventory{ServiceName: OperationInventory()}, b.Build().Inventory())
}

// Every ACM action is implemented: none is Stubbed or Unsupported.
func TestRegistrationInventoryHasNoStubsOrUnsupported(t *testing.T) {
	inv := OperationInventory()
	assert.Len(t, inv.Registered, 9)
	assert.Empty(t, inv.Stubbed)
	assert.Empty(t, inv.Unsupported)
}

func TestRegistrationDeclaresJSONEnvelopeForACM(t *testing.T) {
	reg := NewRegistration(Deps{})
	assert.Equal(t, ServiceName, reg.Service)
	assert.Equal(t, dispatch.ErrorEnvelopeJSON, reg.Errors)

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Amz-Target", "CertificateManager.ListCertificates")
	assert.Equal(t, "ListCertificates", reg.ResolveAction(req))
}

// A denied request must never reach the NATS gate: Authorize runs first, and
// its own error — not ServerInternal from a nil connection — is what the
// caller sees. A permitted request reaches the NATS gate and fails there,
// which is what proves Authorize ran ahead of it.
func TestDispatch_AuthorizesBeforeTheNATSGate(t *testing.T) {
	denied := &fakeInvocation{deny: errors.New(awserrors.ErrorAccessDenied)}
	_, err := dispatchWith(t, Deps{}, denied.invocation("CertificateManager.ListCertificates", "{}"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
	assert.Equal(t, []string{"authorize"}, denied.log)

	permitted := &fakeInvocation{}
	_, err = dispatchWith(t, Deps{}, permitted.invocation("CertificateManager.ListCertificates", "{}"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error(), "authorize passed; the nil NATS connection is what fails next")
	require.Len(t, permitted.auths, 1)
	assert.Equal(t, authCall{
		service:   ServiceName,
		action:    "ListCertificates",
		resources: []string{"*"},
	}, permitted.auths[0])
}

// Requests dispatch rejects before scoping never reach Authorize.
func TestDispatch_RejectsBeforeAuthorize(t *testing.T) {
	cases := []struct {
		name, target, body string
		account            string
		want               string
	}{
		{"absent target", "", "{}", regAccount, awserrors.ErrorMissingAction},
		{"trailing dot", "CertificateManager.", "{}", regAccount, awserrors.ErrorMissingAction},
		{"unknown action", "CertificateManager.MadeUpAction", "{}", regAccount, awserrors.ErrorInvalidAction},
		{"other service's action", "AmazonEC2ContainerServiceV20141113.ListClusters", "{}", regAccount, awserrors.ErrorInvalidAction},
		{"missing account", "CertificateManager.ListCertificates", "{}", "", awserrors.ErrorInternalError},
		{"oversized body", "CertificateManager.ListCertificates", strings.Repeat("x", sigv4.MaxPayloadLen+1), regAccount, awserrors.ErrorRequestEntityTooLarge},
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

// Every registered action answers ServerInternal once authorized, with no
// NATS connection to relay onto.
func TestDispatch_NilNATSIsServerInternal(t *testing.T) {
	for action := range Actions {
		t.Run(action, func(t *testing.T) {
			f := &fakeInvocation{}
			_, err := dispatchWith(t, Deps{}, f.invocation("CertificateManager."+action, "{}"))
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
			assert.Equal(t, []string{"authorize"}, f.log)
		})
	}
}
