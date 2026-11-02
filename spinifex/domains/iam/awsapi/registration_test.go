package awsapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// regService records the calls the dispatcher makes so a test can assert what
// ran, and in which order relative to authorization.
type regService struct {
	stubIAMService

	getUserName   string
	getUserCalls  int
	addRoleCalls  int
	canonicalARN  string
	canonicalErr  error
	accountResult *iamdomain.Account
	accountErr    error
}

func (s *regService) GetUser(_ string, in *iam.GetUserInput) (*iam.GetUserOutput, error) {
	s.getUserCalls++
	if in.UserName != nil {
		s.getUserName = *in.UserName
	}
	return &iam.GetUserOutput{}, nil
}

func (s *regService) AddRoleToInstanceProfile(_ string, _ *iam.AddRoleToInstanceProfileInput) (*iam.AddRoleToInstanceProfileOutput, error) {
	s.addRoleCalls++
	return &iam.AddRoleToInstanceProfileOutput{}, nil
}

// CanonicalResourceARN serves the scope resolver for every kind; only the role
// lookup the PassRole check makes is scripted, the rest resolve like a live store.
func (s *regService) CanonicalResourceARN(accountID string, kind arn.IAMResourceType, name string) (string, error) {
	if kind == arn.IAMRole {
		return s.canonicalARN, s.canonicalErr
	}
	return arn.FormatIAMPath(kind, accountID, "/", name), nil
}

func (s *regService) GetAccount(_ string) (*iamdomain.Account, error) {
	return s.accountResult, s.accountErr
}

type authCall struct {
	service, action string
	resources       []string
	keys            iampolicy.ConditionKeys
}

func invoke(t *testing.T, d Deps, accountID string, actor dispatch.Actor, authorize func(authCall) error, form url.Values) (*httptest.ResponseRecorder, []authCall, error) {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(form.Encode()))
	var calls []authCall
	inv := dispatch.Invocation{
		Request:   r,
		AccountID: accountID,
		Service:   ServiceName,
		Authorize: func(service, action string, resources []string, keys iampolicy.ConditionKeys) error {
			c := authCall{service, action, resources, keys}
			calls = append(calls, c)
			if authorize != nil {
				return authorize(c)
			}
			return nil
		},
		Actor: func() (dispatch.Actor, error) { return actor, nil },
	}
	w := httptest.NewRecorder()
	err := NewRegistration(d).Dispatch(w, inv)
	return w, calls, err
}

func TestRegistration_RequestShapeErrors(t *testing.T) {
	svc := &regService{}
	tests := []struct {
		name string
		deps Deps
		acct string
		form url.Values
		want string
	}{
		{"missing action", Deps{IAM: svc}, testAccountID, url.Values{}, awserrors.ErrorMissingAction},
		{"unknown action", Deps{IAM: svc}, testAccountID, url.Values{"Action": {"NoSuchThing"}}, awserrors.ErrorInvalidAction},
		{"nil service", Deps{}, testAccountID, url.Values{"Action": {"ListUsers"}}, awserrors.ErrorInternalError},
		{"no account", Deps{IAM: svc}, "", url.Values{"Action": {"ListUsers"}}, awserrors.ErrorInternalError},
		{"unknown action beats nil service", Deps{}, testAccountID, url.Values{"Action": {"NoSuchThing"}}, awserrors.ErrorInvalidAction},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w, calls, err := invoke(t, tc.deps, tc.acct, dispatch.Actor{}, nil, tc.form)
			require.Error(t, err)
			assert.True(t, awserrors.IsErrorCode(err, tc.want), "got %v want %s", err, tc.want)
			assert.Empty(t, calls, "no authorization before the request shape is valid")
			assert.Zero(t, w.Body.Len(), "dispatcher writes nothing on error")
		})
	}
}

func TestRegistration_MalformedBody(t *testing.T) {
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader("Action=%G"))
	inv := dispatch.Invocation{Request: r, AccountID: testAccountID}
	err := NewRegistration(Deps{IAM: &regService{}}).Dispatch(httptest.NewRecorder(), inv)
	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorMalformedQueryString))
}

func TestRegistration_ResolveAction(t *testing.T) {
	reg := NewRegistration(Deps{})
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader("Action=ListUsers&Version=2010-05-08"))
	assert.Equal(t, "ListUsers", reg.ResolveAction(r))
	// The dispatcher still reads the body after resolution.
	w, _, err := invokeRequest(t, Deps{IAM: &regService{}}, r)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, w.Code)

	bad := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader("Action=%G"))
	assert.Empty(t, reg.ResolveAction(bad))
}

func invokeRequest(t *testing.T, d Deps, r *http.Request) (*httptest.ResponseRecorder, []authCall, error) {
	t.Helper()
	var calls []authCall
	inv := dispatch.Invocation{
		Request:   r,
		AccountID: testAccountID,
		Authorize: func(service, action string, resources []string, keys iampolicy.ConditionKeys) error {
			calls = append(calls, authCall{service, action, resources, keys})
			return nil
		},
		Actor: func() (dispatch.Actor, error) { return dispatch.Actor{}, nil },
	}
	w := httptest.NewRecorder()
	err := NewRegistration(d).Dispatch(w, inv)
	return w, calls, err
}

func TestRegistration_AuthorizeBeforeHandler(t *testing.T) {
	svc := &regService{}
	deny := errors.New(awserrors.ErrorAccessDenied)
	_, calls, err := invoke(t, Deps{IAM: svc}, testAccountID, dispatch.Actor{},
		func(authCall) error { return deny },
		url.Values{"Action": {"GetUser"}, "UserName": {"alice"}})
	require.ErrorIs(t, err, deny)
	require.Len(t, calls, 1)
	assert.Equal(t, "iam", calls[0].service)
	assert.Equal(t, "GetUser", calls[0].action)
	assert.Equal(t, []string{"arn:aws:iam::" + testAccountID + ":user/alice"}, calls[0].resources)
	assert.Zero(t, svc.getUserCalls, "a denied request must not reach the handler")
}

func TestRegistration_SuccessWritesXML(t *testing.T) {
	svc := &regService{}
	w, calls, err := invoke(t, Deps{IAM: svc}, testAccountID, dispatch.Actor{}, nil,
		url.Values{"Action": {"GetUser"}, "UserName": {"alice"}})
	require.NoError(t, err)
	require.Len(t, calls, 1)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "text/xml", w.Header().Get("Content-Type"))
	assert.Contains(t, w.Body.String(), "GetUserResponse")
	assert.Equal(t, 1, svc.getUserCalls)
}

func TestRegistration_GetUserWithoutName(t *testing.T) {
	root := &iamdomain.Account{CreatedAt: "2024-01-02T03:04:05Z"}
	tests := []struct {
		name         string
		actor        dispatch.Actor
		account      string
		wantErr      string
		wantUserName string
		wantRoot     bool
	}{
		{"user names themselves", dispatch.Actor{PrincipalType: "user", UserName: "alice"}, testAccountID, "", "alice", false},
		{"session token user", dispatch.Actor{PrincipalType: "user", UserName: "bob"}, testAccountID, "", "bob", false},
		{"root principal", dispatch.Actor{PrincipalType: "root"}, testAccountID, "", "", true},
		{"tenant user named root", dispatch.Actor{PrincipalType: "user", UserName: "root"}, "123456789012", "", "root", false},
		{"global user named root is root", dispatch.Actor{PrincipalType: "user", UserName: "root"}, awsidentifiers.GlobalAccountID, "", "", true},
		{"assumed role must name a user", dispatch.Actor{PrincipalType: "assumed-role", SessionName: "s1"}, testAccountID, awserrors.ErrorValidationError, "", false},
		{"unknown principal fails closed", dispatch.Actor{PrincipalType: "mystery"}, testAccountID, awserrors.ErrorInternalError, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := &regService{accountResult: root}
			w, calls, err := invoke(t, Deps{IAM: svc}, tc.account, tc.actor, nil, url.Values{"Action": {"GetUser"}})
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.True(t, awserrors.IsErrorCode(err, tc.wantErr), "got %v", err)
				assert.Empty(t, calls, "defaulting fails before authorization")
				return
			}
			require.NoError(t, err)
			require.Len(t, calls, 1)
			assert.Equal(t, http.StatusOK, w.Code)
			if tc.wantRoot {
				assert.Zero(t, svc.getUserCalls, "root is described without a user record")
				assert.Contains(t, w.Body.String(), tc.account)
				return
			}
			assert.Equal(t, tc.wantUserName, svc.getUserName)
			assert.Equal(t, []string{"arn:aws:iam::" + tc.account + ":user/" + tc.wantUserName}, calls[0].resources)
		})
	}
}

func TestRegistration_AddRoleToInstanceProfilePassRole(t *testing.T) {
	form := url.Values{"Action": {"AddRoleToInstanceProfile"}, "InstanceProfileName": {"p"}, "RoleName": {"r"}}
	roleARN := "arn:aws:iam::" + testAccountID + ":role/svc/r"

	t.Run("passrole checked with the ec2 service key", func(t *testing.T) {
		svc := &regService{canonicalARN: roleARN}
		_, calls, err := invoke(t, Deps{IAM: svc}, testAccountID, dispatch.Actor{}, nil, form)
		require.NoError(t, err)
		require.Len(t, calls, 2)
		assert.Equal(t, "AddRoleToInstanceProfile", calls[0].action)
		assert.Equal(t, "PassRole", calls[1].action)
		assert.Equal(t, []string{roleARN}, calls[1].resources)
		assert.Equal(t, iampolicy.ConditionKeys{iampolicy.KeyPassedToService: "ec2.amazonaws.com"}, calls[1].keys)
		assert.Equal(t, 1, svc.addRoleCalls)
	})

	t.Run("denied passrole blocks the handler", func(t *testing.T) {
		svc := &regService{canonicalARN: roleARN}
		deny := errors.New(awserrors.ErrorAccessDenied)
		_, _, err := invoke(t, Deps{IAM: svc}, testAccountID, dispatch.Actor{},
			func(c authCall) error {
				if c.action == "PassRole" {
					return deny
				}
				return nil
			}, form)
		require.ErrorIs(t, err, deny)
		assert.Zero(t, svc.addRoleCalls)
	})

	t.Run("missing role is still evaluated", func(t *testing.T) {
		svc := &regService{canonicalErr: errors.New(awserrors.ErrorIAMNoSuchEntity)}
		_, calls, err := invoke(t, Deps{IAM: svc}, testAccountID, dispatch.Actor{}, nil, form)
		require.NoError(t, err)
		require.Len(t, calls, 2)
		assert.Equal(t, []string{"arn:aws:iam::" + testAccountID + ":role/r"}, calls[1].resources)
	})

	t.Run("lookup failure fails closed", func(t *testing.T) {
		svc := &regService{canonicalErr: errors.New("boom")}
		_, calls, err := invoke(t, Deps{IAM: svc}, testAccountID, dispatch.Actor{}, nil, form)
		require.Error(t, err)
		assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorInternalError))
		assert.Len(t, calls, 1, "PassRole is never evaluated on a failed lookup")
		assert.Zero(t, svc.addRoleCalls)
	})
}

func TestActionNames_SortedAndInventory(t *testing.T) {
	names := ActionNames()
	require.NotEmpty(t, names)
	assert.IsIncreasing(t, names)
	assert.Equal(t, names, OperationInventory().Registered)
	assert.Contains(t, names, "GetUser")
}

// TestRegistration_EveryActionRoutesToItsHandler sends each action through the
// dispatcher and checks authorization sees that action and, once it passes, the
// action's own handler runs. The service is a stub: a handler that reaches a
// method past the embedded interface panics, which proves it was selected.
func TestRegistration_EveryActionRoutesToItsHandler(t *testing.T) {
	for _, name := range ActionNames() {
		t.Run(name, func(t *testing.T) {
			var authorized []string
			form := url.Values{"Action": {name}, "UserName": {"alice"}}
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(form.Encode()))
			inv := dispatch.Invocation{
				Request:   r,
				AccountID: testAccountID,
				Service:   ServiceName,
				Authorize: func(_, action string, _ []string, _ iampolicy.ConditionKeys) error {
					authorized = append(authorized, action)
					return nil
				},
				Actor: func() (dispatch.Actor, error) {
					return dispatch.Actor{AccountID: testAccountID, PrincipalType: "user", UserName: "alice"}, nil
				},
			}
			handlerReached := false
			func() {
				defer func() {
					if recover() != nil {
						handlerReached = true
					}
				}()
				err := NewRegistration(Deps{IAM: &regService{}}).Dispatch(httptest.NewRecorder(), inv)
				handlerReached = err == nil || !awserrors.IsErrorCode(err, awserrors.ErrorInvalidAction)
			}()
			require.Equal(t, []string{name}, authorized, "authorization sees exactly this action")
			assert.True(t, handlerReached)
		})
	}
}
