package policyauth_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/credential"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/pipeline"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/policyauth"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// authorizeAs runs the pipeline's gate for caller over the real evaluator,
// as the gateway does for a request whose credential has verified.
func authorizeAs(iam iamdomain.IAMService, caller credential.Principal, authenticated bool, service, action string, resources ...string) error {
	if len(resources) == 0 {
		resources = []string{"*"}
	}
	eval := policyauth.New(iam, policyauth.WithRetryBase(time.Millisecond))
	return pipeline.Authorize(eval, httptest.NewRequest(http.MethodPost, "/", nil), caller, authenticated, service, action, resources, nil)
}

func userCaller(identity, accountID string) credential.Principal {
	return credential.Principal{Identity: identity, AccountID: accountID, PrincipalType: gwfix.PrincipalTypeUser}
}

func allowEC2Docs() []iamdomain.PolicyDocument {
	return []iamdomain.PolicyDocument{{
		Version: "2012-10-17",
		Statement: []iamdomain.Statement{
			{Effect: "Allow", Action: iamdomain.StringOrArr{"ec2:*"}, Resource: iamdomain.StringOrArr{"*"}},
		},
	}}
}

// allowAllIAM resolves an allow-everything policy for every principal.
func allowAllIAM() *gwfix.PolicyIAM {
	allow := []iamdomain.PolicyDocument{{
		Version: "2012-10-17",
		Statement: []iamdomain.Statement{
			{Effect: "Allow", Action: iamdomain.StringOrArr{"*"}, Resource: iamdomain.StringOrArr{"*"}},
		},
	}}
	return &gwfix.PolicyIAM{
		GetUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) { return allow, nil },
		GetRolePoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) { return allow, nil },
	}
}

func assertAccessDenied(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	code, ok := awserrors.ResolveErrorCode(err)
	require.True(t, ok, "unresolvable error code: %v", err)
	assert.Equal(t, awserrors.ErrorAccessDenied, code)
}

// TestCheckPolicy_FailsClosed covers the three states that once returned allow
// before any policy was consulted. A missing service is a server fault; a
// request with no resolved principal is denied.
func TestCheckPolicy_FailsClosed(t *testing.T) {
	cases := []struct {
		name          string
		iam           iamdomain.IAMService
		caller        credential.Principal
		authenticated bool
		wantErr       string
	}{
		{"nil IAM service", nil, userCaller("alice", "123456789012"), true, awserrors.ErrorInternalError},
		{"no auth context", &gwfix.IAM{}, credential.Principal{}, false, awserrors.ErrorAccessDenied},
		{"empty identity", &gwfix.IAM{}, userCaller("", "123456789012"), true, awserrors.ErrorAccessDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := authorizeAs(tc.iam, tc.caller, tc.authenticated, "ec2", "DescribeInstances")
			require.Error(t, err)
			assert.Equal(t, tc.wantErr, err.Error())
		})
	}
}

// Shared authorization errors must not expose a canonical resource that was
// resolved from storage. STS opts into detailed wording at its dispatcher.
func TestCheckPolicyResources_DenialIsOpaque(t *testing.T) {
	mock := &gwfix.PolicyIAM{
		GetUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) {
			return nil, nil
		},
	}
	resource := "arn:aws:iam::123456789012:role/private/target"
	err := authorizeAs(mock, userCaller("alice", "123456789012"), true, "iam", "GetRole", resource)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())

	code, message, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorAccessDenied, code)
	assert.Empty(t, message)
	assert.NotContains(t, err.Error(), resource)
}

// TestCheckPolicy_PassRoleFailsClosed proves the change reaches a real gated
// call site: an unauthenticated RunInstances carrying an instance profile is
// denied rather than passing the iam:PassRole check.
func TestCheckPolicy_PassRoleFailsClosed(t *testing.T) {
	err := authorizeAs(allowAllIAM(), credential.Principal{}, false, "iam", "PassRole",
		"arn:aws:iam::123456789012:role/app")
	require.Error(t, err)
	assertAccessDenied(t, err)
}

func TestCheckPolicy_RootUserGlobalAccount(t *testing.T) {
	err := authorizeAs(&gwfix.IAM{}, userCaller("root", "000000000000"), true, "ec2", "DescribeInstances")
	assert.NoError(t, err)
}

// TestCheckPolicy_AssumedRoleSessionNamedRoot ensures the principal-type gate
// fires BEFORE the identity-string root short-circuit. A session whose
// SessionName is "root" must not inherit root privileges.
func TestCheckPolicy_AssumedRoleSessionNamedRoot(t *testing.T) {
	caller := credential.Principal{
		Identity: "root", AccountID: "000000000000", PrincipalType: gwfix.PrincipalTypeAssumedRole,
		AssumedRoleARN: "arn:aws:sts::000000000000:assumed-role/r/root",
	}
	err := authorizeAs(&gwfix.IAM{}, caller, true, "ec2", "DescribeInstances")
	require.Error(t, err)
	assertAccessDenied(t, err)
}

func TestCheckPolicy_NonRootAllowPolicy(t *testing.T) {
	mock := &gwfix.PolicyIAM{
		GetUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) { return allowEC2Docs(), nil },
	}
	err := authorizeAs(mock, userCaller("alice", "123456789012"), true, "ec2", "DescribeInstances")
	assert.NoError(t, err)
}

func TestCheckPolicy_NonRootDenyPolicy(t *testing.T) {
	mock := &gwfix.PolicyIAM{
		GetUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) {
			return []iamdomain.PolicyDocument{{
				Version: "2012-10-17",
				Statement: []iamdomain.Statement{
					{Effect: "Deny", Action: iamdomain.StringOrArr{"ec2:*"}, Resource: iamdomain.StringOrArr{"*"}},
				},
			}}, nil
		},
	}
	err := authorizeAs(mock, userCaller("alice", "123456789012"), true, "ec2", "DescribeInstances")
	require.Error(t, err)
	assertAccessDenied(t, err)
}

func TestCheckPolicy_NonRootNoPolicies(t *testing.T) {
	mock := &gwfix.PolicyIAM{
		GetUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) { return nil, nil },
	}
	err := authorizeAs(mock, userCaller("alice", "123456789012"), true, "ec2", "DescribeInstances")
	require.Error(t, err)
	assertAccessDenied(t, err)
}

func TestCheckPolicy_GetUserPoliciesError(t *testing.T) {
	mock := &gwfix.PolicyIAM{
		GetUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) {
			return nil, errors.New("db connection failed")
		},
	}
	err := authorizeAs(mock, userCaller("alice", "123456789012"), true, "ec2", "DescribeInstances")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInternalError, err.Error())
}

func TestCheckPolicy_MissingAccountID(t *testing.T) {
	err := authorizeAs(&gwfix.IAM{}, credential.Principal{Identity: "alice"}, true, "ec2", "DescribeInstances")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInternalError, err.Error())
}

func TestCheckPolicy_NATSTransientRetriesAllAttempts(t *testing.T) {
	calls := 0
	mock := &gwfix.PolicyIAM{
		GetUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) {
			calls++
			return nil, fmt.Errorf("get user: %w", nats.ErrNoResponders)
		},
	}
	err := authorizeAs(mock, userCaller("alice", "123456789012"), true, "ec2", "DescribeInstances")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInternalError, err.Error())
	assert.Equal(t, 3, calls, "should have retried all 3 attempts")
}

func TestCheckPolicy_NATSTransientRetriesThenSucceeds(t *testing.T) {
	calls := 0
	mock := &gwfix.PolicyIAM{
		GetUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) {
			calls++
			if calls < 3 {
				return nil, fmt.Errorf("get user: %w", nats.ErrNoResponders)
			}
			return allowEC2Docs(), nil
		},
	}
	err := authorizeAs(mock, userCaller("alice", "123456789012"), true, "ec2", "DescribeInstances")
	assert.NoError(t, err)
	assert.Equal(t, 3, calls, "should have retried until success")
}

func TestCheckPolicy_NonTransientErrorStillFails(t *testing.T) {
	mock := &gwfix.PolicyIAM{
		GetUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) {
			return nil, errors.New("database corruption")
		},
	}
	err := authorizeAs(mock, userCaller("alice", "123456789012"), true, "ec2", "DescribeInstances")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInternalError, err.Error())
}
