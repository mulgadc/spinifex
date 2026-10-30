package policyauth

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/credential"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeIAM embeds the interface so an unexpected call panics.
type fakeIAM struct {
	iamdomain.IAMService

	userPolicies func(accountID, user string) ([]iamdomain.PolicyDocument, error)
	rolePolicies func(accountID, role string) ([]iamdomain.PolicyDocument, error)
}

func (f *fakeIAM) GetUserPolicies(a, u string) ([]iamdomain.PolicyDocument, error) {
	return f.userPolicies(a, u)
}

func (f *fakeIAM) GetRolePolicies(a, r string) ([]iamdomain.PolicyDocument, error) {
	return f.rolePolicies(a, r)
}

func policies(effect, action, resource string) []iamdomain.PolicyDocument {
	return []iamdomain.PolicyDocument{{
		Version: "2012-10-17",
		Statement: []iamdomain.Statement{
			{Effect: effect, Action: iamdomain.StringOrArr{action}, Resource: iamdomain.StringOrArr{resource}},
		},
	}}
}

const acct = "000000000001"

var (
	alice = credential.Principal{Identity: "alice", AccountID: acct, PrincipalType: stsdomain.PrincipalTypeUser}
	role  = credential.Principal{
		Identity: "session", AccountID: acct, PrincipalType: stsdomain.PrincipalTypeAssumedRole,
		AssumedRoleARN:    "arn:aws:sts::" + acct + ":assumed-role/Ops/session",
		UnderlyingRoleARN: "arn:aws:iam::" + acct + ":role/Ops",
	}
)

func TestEvaluate_UserAllowAndDeny(t *testing.T) {
	allow := New(&fakeIAM{userPolicies: func(string, string) ([]iamdomain.PolicyDocument, error) {
		return policies("Allow", "ecr:*", "*"), nil
	}})
	require.NoError(t, allow.Evaluate(alice, "ecr:PutImage", []string{"arn:aws:ecr:us-east-1:" + acct + ":repository/a"}, nil))

	deny := New(&fakeIAM{userPolicies: func(string, string) ([]iamdomain.PolicyDocument, error) {
		return policies("Deny", "ecr:*", "*"), nil
	}})
	err := deny.Evaluate(alice, "ecr:PutImage", []string{"r1", "r2"}, nil)
	var denial *DenialError
	require.ErrorAs(t, err, &denial)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
	assert.Equal(t, "ecr:PutImage", denial.Action)
	assert.Equal(t, "r1", denial.Resource, "the first denied resource is reported")
	var policyDenial *dispatch.PolicyDenialError
	require.ErrorAs(t, err, &policyDenial)
	assert.Equal(t, "r1", policyDenial.Resource)
}

func TestEvaluate_NoPoliciesIsImplicitDeny(t *testing.T) {
	e := New(&fakeIAM{userPolicies: func(string, string) ([]iamdomain.PolicyDocument, error) { return nil, nil }})
	var denial *DenialError
	require.ErrorAs(t, e.Evaluate(alice, "ecr:GetAuthorizationToken", []string{"*"}, nil), &denial)
}

func TestEvaluate_GlobalRootBypassIsUserOnly(t *testing.T) {
	e := New(&fakeIAM{
		userPolicies: func(string, string) ([]iamdomain.PolicyDocument, error) {
			t.Fatal("global root must not read policies")
			return nil, nil
		},
		rolePolicies: func(string, string) ([]iamdomain.PolicyDocument, error) { return nil, nil },
	})
	root := credential.Principal{Identity: "root", AccountID: awsidentifiers.GlobalAccountID, PrincipalType: stsdomain.PrincipalTypeUser}
	require.NoError(t, e.Evaluate(root, "ecr:PutImage", []string{"*"}, nil))

	// A role session named "root" in the same account never reaches the bypass.
	sessionRoot := credential.Principal{
		Identity: "root", AccountID: awsidentifiers.GlobalAccountID, PrincipalType: stsdomain.PrincipalTypeAssumedRole,
		UnderlyingRoleARN: "arn:aws:iam::" + awsidentifiers.GlobalAccountID + ":role/Ops",
	}
	var denial *DenialError
	require.ErrorAs(t, e.Evaluate(sessionRoot, "ecr:PutImage", []string{"*"}, nil), &denial)
}

func TestEvaluate_AssumedRoleResolvesByUnderlyingRole(t *testing.T) {
	var gotRole string
	e := New(&fakeIAM{rolePolicies: func(_, name string) ([]iamdomain.PolicyDocument, error) {
		gotRole = name
		return policies("Allow", "ecr:*", "*"), nil
	}})
	require.NoError(t, e.Evaluate(role, "ecr:BatchGetImage", []string{"*"}, nil))
	assert.Equal(t, "Ops", gotRole)
}

func TestEvaluate_FailsClosed(t *testing.T) {
	e := New(&fakeIAM{})
	crossAccount := role
	crossAccount.UnderlyingRoleARN = "arn:aws:iam::000000000002:role/Ops"
	noRole := role
	noRole.UnderlyingRoleARN = ""

	cases := []struct {
		name string
		e    *Evaluator
		p    credential.Principal
		code string
	}{
		{"nil evaluator", nil, alice, awserrors.ErrorInternalError},
		{"nil IAM", New(nil), alice, awserrors.ErrorInternalError},
		{"cross-account role", e, crossAccount, awserrors.ErrorAccessDenied},
		{"legacy role without ARN", e, noRole, awserrors.ErrorAccessDenied},
		{"unknown principal type", e, credential.Principal{Identity: "x", AccountID: acct, PrincipalType: "federated"}, awserrors.ErrorInternalError},
		{"policy read fault", New(&fakeIAM{userPolicies: func(string, string) ([]iamdomain.PolicyDocument, error) {
			return nil, errors.New("kv down")
		}}), alice, awserrors.ErrorInternalError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.e.Evaluate(tc.p, "ecr:PutImage", []string{"*"}, nil)
			require.Error(t, err)
			assert.Equal(t, tc.code, err.Error())
		})
	}
}

func TestEvaluate_RetriesTransientNATS(t *testing.T) {
	calls := 0
	e := New(&fakeIAM{userPolicies: func(string, string) ([]iamdomain.PolicyDocument, error) {
		calls++
		if calls < 3 {
			return nil, fmt.Errorf("get user: %w", nats.ErrNoResponders)
		}
		return policies("Allow", "ecr:*", "*"), nil
	}}, WithRetryBase(time.Millisecond))
	require.NoError(t, e.Evaluate(alice, "ecr:PutImage", []string{"*"}, nil))
	assert.Equal(t, 3, calls)

	calls = 0
	always := New(&fakeIAM{userPolicies: func(string, string) ([]iamdomain.PolicyDocument, error) {
		calls++
		return nil, nats.ErrTimeout
	}}, WithRetryBase(time.Millisecond))
	err := always.Evaluate(alice, "ecr:PutImage", []string{"*"}, nil)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInternalError, err.Error())
	assert.Equal(t, 3, calls, "a persistent transient fault stops after three attempts")
}

func TestIsNATSTransient(t *testing.T) {
	assert.False(t, isNATSTransient(nil))
	assert.True(t, isNATSTransient(nats.ErrNoResponders))
	assert.True(t, isNATSTransient(nats.ErrNoStreamResponse))
	assert.True(t, isNATSTransient(fmt.Errorf("get user: %w", nats.ErrTimeout)))
	assert.False(t, isNATSTransient(errors.New("other")))
}

func TestRequestConditionKeys(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.TLS = &tls.ConnectionState{}
	r.RemoteAddr = "10.4.1.9:52344"

	user := alice
	user.UserID = "AIDAALICE"
	keys := RequestConditionKeys(r, user)
	assert.Equal(t, "true", keys[iampolicy.KeySecureTransport])
	assert.Equal(t, "alice", keys[iampolicy.KeyUsername])
	assert.Equal(t, acct, keys[iampolicy.KeyPrincipalAccount])
	assert.Equal(t, "AIDAALICE", keys[iampolicy.KeyUserID])
	assert.Equal(t, "10.4.1.9", keys[iampolicy.KeySourceIP])
	assert.Equal(t, iampolicy.PrincipalTypeUser, keys[iampolicy.KeyPrincipalType])
	assert.Contains(t, keys, iampolicy.KeyCurrentTime)
	assert.Contains(t, keys, iampolicy.KeyEpochTime)
}

func TestRequestConditionKeys_OmitsWhatItCannotResolve(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.RemoteAddr = ""

	keys := RequestConditionKeys(r, role)
	assert.Equal(t, "false", keys[iampolicy.KeySecureTransport])
	assert.NotContains(t, keys, iampolicy.KeyUsername, "a role session's name is caller-chosen")
	assert.NotContains(t, keys, iampolicy.KeyUserID)
	assert.NotContains(t, keys, iampolicy.KeySourceIP)
	assert.Equal(t, iampolicy.PrincipalTypeAssumedRole, keys[iampolicy.KeyPrincipalType])

	empty := RequestConditionKeys(r, credential.Principal{})
	assert.NotContains(t, empty, iampolicy.KeyPrincipalType)
	assert.NotContains(t, empty, iampolicy.KeyPrincipalAccount)

	rootKeys := RequestConditionKeys(r, credential.Principal{PrincipalType: stsdomain.PrincipalTypeRoot})
	assert.Equal(t, iampolicy.PrincipalTypeAccount, rootKeys[iampolicy.KeyPrincipalType])
}
