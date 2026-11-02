package awsapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	regAccount = "000000000001"
	regRoleARN = "arn:aws:iam::000000000001:role/demo"
)

// roleIAM scripts the one IAM call the AssumeRole identity gate makes.
type roleIAM struct {
	stubIAMService

	getRole func(accountID string, in *iam.GetRoleInput) (*iam.GetRoleOutput, error)
}

func (r *roleIAM) GetRole(accountID string, in *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
	return r.getRole(accountID, in)
}

// webIdentitySTS answers the one unsigned action.
type webIdentitySTS struct{ stubSTSService }

func (webIdentitySTS) AssumeRoleWithWebIdentity(*sts.AssumeRoleWithWebIdentityInput) (*sts.AssumeRoleWithWebIdentityOutput, error) {
	return &sts.AssumeRoleWithWebIdentityOutput{}, nil
}

type regAuthCall struct {
	service, action string
	resources       []string
}

func invoke(t *testing.T, d Deps, accountID string, actor dispatch.Actor, actorErr error, authorize func() error, body string) (*httptest.ResponseRecorder, []regAuthCall, error) {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(body))
	var calls []regAuthCall
	inv := dispatch.Invocation{
		Request:   r,
		AccountID: accountID,
		Service:   ServiceName,
		Authorize: func(service, action string, resources []string, _ iampolicy.ConditionKeys) error {
			calls = append(calls, regAuthCall{service, action, resources})
			if authorize != nil {
				return authorize()
			}
			return nil
		},
		Actor: func() (dispatch.Actor, error) { return actor, actorErr },
	}
	w := httptest.NewRecorder()
	err := NewRegistration(d).Dispatch(w, inv)
	return w, calls, err
}

func assumeRoleBody(roleARN string) string {
	v := url.Values{"Action": {"AssumeRole"}, "RoleSessionName": {"s1"}}
	if roleARN != "" {
		v.Set("RoleArn", roleARN)
	}
	return v.Encode()
}

func foundRole(string, *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
	return &iam.GetRoleOutput{Role: &iam.Role{Arn: aws.String(regRoleARN)}}, nil
}

func userActor() dispatch.Actor {
	return dispatch.Actor{
		AccountID: regAccount, CallerARN: "arn:aws:iam::000000000001:user/alice",
		PrincipalType: "user", UserName: "alice", AccessKeyID: "AKIA1",
	}
}

func TestRegistration_RequestShapeErrors(t *testing.T) {
	tests := []struct {
		name string
		deps Deps
		body string
		want string
	}{
		{"missing action", Deps{STS: &stubSTSService{}}, "", awserrors.ErrorMissingAction},
		{"unknown action", Deps{STS: &stubSTSService{}}, "Action=Nope", awserrors.ErrorInvalidAction},
		{"nil service", Deps{}, "Action=GetCallerIdentity", awserrors.ErrorInternalError},
		{"unknown action beats nil service", Deps{}, "Action=Nope", awserrors.ErrorInvalidAction},
		{"malformed body", Deps{STS: &stubSTSService{}}, "Action=%G", awserrors.ErrorMalformedQueryString},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w, calls, err := invoke(t, tc.deps, regAccount, userActor(), nil, nil, tc.body)
			require.Error(t, err)
			assert.True(t, awserrors.IsErrorCode(err, tc.want), "got %v want %s", err, tc.want)
			assert.Empty(t, calls)
			assert.Zero(t, w.Body.Len(), "dispatcher writes nothing on error")
		})
	}
}

func TestRegistration_CallerErrors(t *testing.T) {
	deps := Deps{STS: &stubSTSService{}}
	t.Run("no account", func(t *testing.T) {
		_, _, err := invoke(t, deps, regAccount, dispatch.Actor{}, errors.New("no caller"), nil, "Action=GetCallerIdentity")
		assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorInternalError))
	})
	t.Run("actor fault", func(t *testing.T) {
		a := userActor()
		_, _, err := invoke(t, deps, regAccount, a, errors.New("boom"), nil, "Action=GetCallerIdentity")
		require.Error(t, err)
		assert.False(t, awserrors.IsErrorCode(err, awserrors.ErrorInternalError))
	})
}

func TestRegistration_GetCallerIdentityReadsActor(t *testing.T) {
	var gotARN, gotUserID string
	svc := &stubSTSService{getCallerIdentityFn: func(acct, arn, uid string, _ *sts.GetCallerIdentityInput) (*sts.GetCallerIdentityOutput, error) {
		gotARN, gotUserID = arn, uid
		return &sts.GetCallerIdentityOutput{Account: aws.String(acct), Arn: aws.String(arn), UserId: aws.String(uid)}, nil
	}}
	t.Run("assumed role", func(t *testing.T) {
		a := dispatch.Actor{
			AccountID: regAccount, CallerARN: "arn:aws:sts::000000000001:assumed-role/demo/s1",
			PrincipalType: principalTypeAssumedRole, SessionName: "s1", UserID: "AROAX:s1",
		}
		w, calls, err := invoke(t, Deps{STS: svc, IAM: &stubIAMService{}}, regAccount, a, nil, nil, "Action=GetCallerIdentity")
		require.NoError(t, err)
		assert.Empty(t, calls, "GetCallerIdentity is not identity-gated")
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "text/xml", w.Header().Get("Content-Type"))
		assert.Contains(t, w.Body.String(), "GetCallerIdentityResult")
		assert.Equal(t, a.CallerARN, gotARN)
		assert.Equal(t, "AROAX:s1", gotUserID)
	})
}

func TestRegistration_AssumeRoleGate(t *testing.T) {
	assumed := 0
	svc := &stubSTSService{assumeRoleFn: func(string, string, string, *sts.AssumeRoleInput) (*sts.AssumeRoleOutput, error) {
		assumed++
		return &sts.AssumeRoleOutput{}, nil
	}}
	iamOK := &roleIAM{getRole: foundRole}

	t.Run("allowed runs the handler against the stored role ARN", func(t *testing.T) {
		assumed = 0
		w, calls, err := invoke(t, Deps{STS: svc, IAM: iamOK}, regAccount, userActor(), nil, nil, assumeRoleBody(regRoleARN))
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, 1, assumed)
		require.Len(t, calls, 1)
		assert.Equal(t, regAuthCall{"sts", "AssumeRole", []string{regRoleARN}}, calls[0])
	})

	t.Run("denial names caller action and resource", func(t *testing.T) {
		assumed = 0
		deny := func() error { return &dispatch.PolicyDenialError{Action: "sts:AssumeRole", Resource: regRoleARN} }
		_, _, err := invoke(t, Deps{STS: svc, IAM: iamOK}, regAccount, userActor(), nil, deny, assumeRoleBody(regRoleARN))
		require.Error(t, err)
		assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorAccessDenied))
		assert.Contains(t, err.Error(), "arn:aws:iam::000000000001:user/alice")
		assert.Contains(t, err.Error(), "sts:AssumeRole")
		assert.Contains(t, err.Error(), regRoleARN)
		assert.Zero(t, assumed, "handler must not run after a denial")
	})

	t.Run("opaque denial passes through", func(t *testing.T) {
		deny := func() error { return errors.New(awserrors.ErrorAccessDenied) }
		_, _, err := invoke(t, Deps{STS: svc, IAM: iamOK}, regAccount, userActor(), nil, deny, assumeRoleBody(regRoleARN))
		assert.EqualError(t, err, awserrors.ErrorAccessDenied)
	})

	t.Run("missing RoleArn is a missing parameter before authorization", func(t *testing.T) {
		_, calls, err := invoke(t, Deps{STS: svc, IAM: iamOK}, regAccount, userActor(), nil, nil, assumeRoleBody(""))
		assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorMissingParameter))
		assert.Empty(t, calls)
	})

	t.Run("nil IAM", func(t *testing.T) {
		_, calls, err := invoke(t, Deps{STS: svc}, regAccount, userActor(), nil, nil, assumeRoleBody(regRoleARN))
		assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorInternalError))
		assert.Empty(t, calls)
	})

	t.Run("unresolved role echoes the supplied ARN", func(t *testing.T) {
		missing := &roleIAM{getRole: func(string, *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
			return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
		}}
		_, calls, err := invoke(t, Deps{STS: svc, IAM: missing}, regAccount, userActor(), nil, nil, assumeRoleBody(regRoleARN))
		require.NoError(t, err)
		require.Len(t, calls, 1)
		assert.Equal(t, []string{regRoleARN}, calls[0].resources)
	})

	t.Run("unparseable RoleArn is a validation error", func(t *testing.T) {
		_, calls, err := invoke(t, Deps{STS: svc, IAM: iamOK}, regAccount, userActor(), nil, nil, assumeRoleBody("not-an-arn"))
		assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorValidationError))
		assert.Empty(t, calls)
	})

	t.Run("lookup fault is internal", func(t *testing.T) {
		broken := &roleIAM{getRole: func(string, *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
			return nil, errors.New("kv down")
		}}
		_, calls, err := invoke(t, Deps{STS: svc, IAM: broken}, regAccount, userActor(), nil, nil, assumeRoleBody(regRoleARN))
		assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorInternalError))
		assert.Empty(t, calls)
	})
}

func TestDetailedDenial_CallerFallback(t *testing.T) {
	denial := &dispatch.PolicyDenialError{Action: "sts:AssumeRole", Resource: regRoleARN}
	t.Run("user without ARN names the user", func(t *testing.T) {
		err := detailedDenial(dispatch.Actor{UserName: "alice", PrincipalType: "user"}, denial)
		assert.Contains(t, err.Error(), "User: alice is not")
	})
	t.Run("session without ARN names the session", func(t *testing.T) {
		err := detailedDenial(dispatch.Actor{SessionName: "s1", PrincipalType: principalTypeAssumedRole}, denial)
		assert.Contains(t, err.Error(), "User: s1 is not")
	})
}

func TestRegistration_GetSessionTokenForwardsActor(t *testing.T) {
	var got [4]string
	svc := &stubSTSService{getSessionTokenFn: func(acct, name, pt, akid string, _ *sts.GetSessionTokenInput) (*sts.GetSessionTokenOutput, error) {
		got = [4]string{acct, name, pt, akid}
		return &sts.GetSessionTokenOutput{}, nil
	}}
	_, _, err := invoke(t, Deps{STS: svc}, regAccount, userActor(), nil, nil, "Action=GetSessionToken")
	require.NoError(t, err)
	assert.Equal(t, [4]string{regAccount, "alice", "user", "AKIA1"}, got)
}

func TestRegistration_UnparseableInputIsValidationError(t *testing.T) {
	// A non-numeric DurationSeconds cannot be read into the typed input.
	body := assumeRoleBody(regRoleARN) + "&DurationSeconds=abc"
	_, calls, err := invoke(t, Deps{STS: &stubSTSService{}, IAM: &roleIAM{getRole: foundRole}}, regAccount, userActor(), nil, nil, body)
	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorValidationError), "got %v", err)
	require.Len(t, calls, 1, "authorization precedes input parsing, as before")
}

func TestRegistration_ResolveAction(t *testing.T) {
	reg := NewRegistration(Deps{})
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader("Action=GetCallerIdentity&Version=2011-06-15"))
	assert.Equal(t, "GetCallerIdentity", reg.ResolveAction(r))
	w, _, err := invokeRequest(t, Deps{STS: &stubSTSService{}, IAM: &stubIAMService{}}, r)
	require.NoError(t, err, "dispatcher still reads the body after resolution")
	assert.Equal(t, http.StatusOK, w.Code)

	bad := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader("Action=%G"))
	assert.Empty(t, reg.ResolveAction(bad))
}

func invokeRequest(t *testing.T, d Deps, r *http.Request) (*httptest.ResponseRecorder, []regAuthCall, error) {
	t.Helper()
	inv := dispatch.Invocation{
		Request:   r,
		AccountID: regAccount,
		Authorize: func(string, string, []string, iampolicy.ConditionKeys) error { return nil },
		Actor:     func() (dispatch.Actor, error) { return userActor(), nil },
	}
	w := httptest.NewRecorder()
	return w, nil, NewRegistration(d).Dispatch(w, inv)
}

func TestActionNames_AreTheServedSet(t *testing.T) {
	assert.Equal(t,
		[]string{"AssumeRole", "AssumeRoleWithWebIdentity", "GetAccessKeyInfo", "GetCallerIdentity", "GetSessionToken"},
		ActionNames())
	assert.Equal(t, ActionNames(), OperationInventory().Registered)
}

func TestAnonymousAssumeRoleWithWebIdentity(t *testing.T) {
	q := map[string]string{
		"Action": "AssumeRoleWithWebIdentity", "RoleArn": regRoleARN,
		"RoleSessionName": "s1", "WebIdentityToken": "h.p.s",
	}
	t.Run("nil service fails closed", func(t *testing.T) {
		_, err := NewAnonymousAssumeRoleWithWebIdentity(Deps{})(q)
		assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorInternalError))
	})
	t.Run("runs the handler with no caller", func(t *testing.T) {
		svc := &webIdentitySTS{}
		out, err := NewAnonymousAssumeRoleWithWebIdentity(Deps{STS: svc})(q)
		require.NoError(t, err)
		assert.Contains(t, string(out), "AssumeRoleWithWebIdentityResult")
	})
}
