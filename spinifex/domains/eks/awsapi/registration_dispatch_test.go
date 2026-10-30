package awsapi_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	"github.com/mulgadc/bluebottle/pkg/sigv4"
	eksawsapi "github.com/mulgadc/spinifex/spinifex/domains/eks/awsapi"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/passrole"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	dispatchAccount = "123456789012"
	existingRole    = "arn:aws:iam::123456789012:role/eks-role"
)

// rolesByARN resolves the listed roles and reports every other own-account
// role as missing.
type rolesByARN map[string]bool

func (m rolesByARN) ResolveRole(accountID, roleARN string) (passrole.Role, error) {
	prefix := "arn:aws:iam::" + accountID + ":role/"
	if !strings.HasPrefix(roleARN, prefix) {
		if strings.HasPrefix(roleARN, "arn:aws:iam::") {
			return passrole.Role{}, passrole.ErrForeignAccount
		}
		return passrole.Role{}, passrole.ErrMalformed
	}
	if !m[roleARN] {
		return passrole.Role{}, passrole.ErrNotFound
	}
	return passrole.Role{ARN: roleARN, Name: strings.TrimPrefix(roleARN, prefix)}, nil
}

type authCall struct{ service, action string }

func eksInvoke(t *testing.T, method, path, body, account string, deny error) ([]authCall, []string, error) {
	t.Helper()
	var calls []authCall
	var passed []string
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	inv := dispatch.Invocation{
		Request:   req,
		AccountID: account,
		Region:    "us-east-1",
		Authorize: func(service, action string, resources []string, _ iampolicy.ConditionKeys) error {
			calls = append(calls, authCall{service, action})
			if service == "iam" {
				passed = append(passed, resources...)
				return nil
			}
			return deny
		},
		Actor: func() (dispatch.Actor, error) {
			return dispatch.Actor{AccountID: account, PrincipalType: "user", UserName: "alice"}, nil
		},
	}
	deps := eksawsapi.Deps{Roles: rolesByARN{existingRole: true}}
	err := eksawsapi.NewRegistration(deps).Dispatch(httptest.NewRecorder(), inv)
	return calls, passed, err
}

func TestRegistration_ResolvesRoutes(t *testing.T) {
	reg := eksawsapi.NewRegistration(eksawsapi.Deps{})
	assert.Equal(t, "eks", reg.Service)
	for _, tt := range []struct{ method, path, want string }{
		{http.MethodGet, "/clusters", "ListClusters"},
		{http.MethodPost, "/clusters", "CreateCluster"},
		{http.MethodGet, "/no/such/route", ""},
	} {
		req := httptest.NewRequest(tt.method, tt.path, nil)
		assert.Equal(t, tt.want, reg.ResolveAction(req), tt.method+" "+tt.path)
	}
	assert.Contains(t, eksawsapi.OperationInventory().Registered, "CreateCluster")
}

func TestRegistration_RequestShapeErrors(t *testing.T) {
	tests := []struct {
		name, method, path, account, want string
	}{
		{"no route", http.MethodGet, "/no/such/route", dispatchAccount, awserrors.ErrorInvalidAction},
		{"no account", http.MethodGet, "/clusters", "", awserrors.ErrorInternalError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := eksInvoke(t, tt.method, tt.path, "", tt.account, nil)
			code, _ := awserrors.ResolveErrorCode(err)
			assert.Equal(t, tt.want, code)
		})
	}
}

func TestRegistration_OversizedBodyIsRefused(t *testing.T) {
	body := strings.Repeat("a", int(sigv4.MaxPayloadLen)+1)
	_, _, err := eksInvoke(t, http.MethodPost, "/clusters", body, dispatchAccount, nil)
	code, _ := awserrors.ResolveErrorCode(err)
	assert.Equal(t, awserrors.ErrorRequestEntityTooLarge, code)
}

func TestRegistration_DenialStopsBeforePassRole(t *testing.T) {
	deny := errors.New("denied")
	calls, passed, err := eksInvoke(t, http.MethodPost, "/clusters", `{"name":"c","roleArn":"`+existingRole+`"}`, dispatchAccount, deny)

	require.ErrorIs(t, err, deny)
	assert.Equal(t, []authCall{{"eks", "CreateCluster"}}, calls)
	assert.Empty(t, passed)
}

func TestRegistration_PassRoleCases(t *testing.T) {
	missing := "arn:aws:iam::123456789012:role/missing"
	tests := []struct {
		name, body, wantCode, wantMsg string
		wantPassed                    []string
	}{
		{"foreign role", `{"name":"c","roleArn":"arn:aws:iam::999999999999:role/r"}`, awserrors.ErrorAccessDeniedException, "Cross-account pass role is not allowed.", nil},
		{"not an ARN", `{"name":"c","roleArn":"x"}`, awserrors.ErrorAccessDeniedException, "Cross-account pass role is not allowed.", nil},
		{"missing role is authorized before it is reported", `{"name":"c","roleArn":"` + missing + `"}`, awserrors.ErrorInvalidParameterValue, "does not exist", []string{missing}},
		{"existing role reaches the daemon", `{"name":"c","roleArn":"` + existingRole + `"}`, awserrors.ErrorServerInternal, "", []string{existingRole}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, passed, err := eksInvoke(t, http.MethodPost, "/clusters", tt.body, dispatchAccount, nil)

			require.Error(t, err)
			code, _ := awserrors.ResolveErrorCode(err)
			assert.Equal(t, tt.wantCode, code)
			if tt.wantMsg != "" {
				assert.Contains(t, err.Error(), tt.wantMsg)
			}
			assert.Equal(t, tt.wantPassed, passed)
		})
	}
}

func TestPassedRoleARNs(t *testing.T) {
	tests := []struct {
		action, body string
		want         []string
	}{
		{"CreateCluster", `{"roleArn":"r1"}`, []string{"r1"}},
		{"CreateNodegroup", `{"nodeRole":"r2"}`, []string{"r2"}},
		{"CreateAddon", `{"serviceAccountRoleArn":"r3"}`, []string{"r3"}},
		{"UpdateAddon", `{"serviceAccountRoleArn":"r4"}`, []string{"r4"}},
		{"CreateCluster", `{"name":"c"}`, nil},
		{"CreateCluster", `{`, nil},
		{"ListClusters", `{"roleArn":"r5"}`, nil},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, eksawsapi.PassedRoleARNs(tt.action, []byte(tt.body)), tt.action+" "+tt.body)
	}
}

func TestCallerFromActor_RoleNameComesFromTheRoleNotTheSession(t *testing.T) {
	actor := dispatch.Actor{
		AccountID:     dispatchAccount,
		PrincipalType: "assumed-role",
		SessionName:   "i-123",
		PrincipalARN:  "arn:aws:iam::123456789012:role/real-role",
	}
	caller := eksawsapi.CallerFromActor(actor)

	assert.Equal(t, "i-123", caller.SessionName)
	assert.Equal(t, "real-role", caller.RoleName)

	user := eksawsapi.CallerFromActor(dispatch.Actor{AccountID: dispatchAccount, PrincipalType: "user", UserName: "alice"})
	assert.Equal(t, "alice", user.SessionName)
	assert.Empty(t, user.RoleName)
}
