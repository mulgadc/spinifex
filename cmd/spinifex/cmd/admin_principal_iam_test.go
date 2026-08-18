package cmd_test

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/cmd/spinifex/cmd"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const superAdminAccountID = "000000000001"

// principalIAMStub embeds the IAMService interface so only the calls the
// principal commands make are implemented. Anything else nil-panics, which is
// the point: an unexpected call is a change in behaviour, not a passing test.
type principalIAMStub struct {
	handlers_iam.IAMService

	inlinePolicies   []string
	inlineDocument   string
	attachedPolicies []string
	accessKeys       []string

	deletedPolicies []string
	deletedKeys     []string
	listKeysErr     error
}

func (s *principalIAMStub) ListUserPolicies(_ string, _ *iam.ListUserPoliciesInput) (*iam.ListUserPoliciesOutput, error) {
	return &iam.ListUserPoliciesOutput{PolicyNames: aws.StringSlice(s.inlinePolicies)}, nil
}

func (s *principalIAMStub) DeleteUserPolicy(_ string, input *iam.DeleteUserPolicyInput) (*iam.DeleteUserPolicyOutput, error) {
	s.deletedPolicies = append(s.deletedPolicies, aws.StringValue(input.PolicyName))
	return &iam.DeleteUserPolicyOutput{}, nil
}

func (s *principalIAMStub) GetUserPolicy(_ string, input *iam.GetUserPolicyInput) (*iam.GetUserPolicyOutput, error) {
	if s.inlineDocument == "" {
		return nil, errors.New("NoSuchEntity")
	}
	return &iam.GetUserPolicyOutput{
		PolicyName:     input.PolicyName,
		PolicyDocument: aws.String(s.inlineDocument),
	}, nil
}

func (s *principalIAMStub) ListAttachedUserPolicies(_ string, _ *iam.ListAttachedUserPoliciesInput) (*iam.ListAttachedUserPoliciesOutput, error) {
	out := &iam.ListAttachedUserPoliciesOutput{}
	for _, name := range s.attachedPolicies {
		out.AttachedPolicies = append(out.AttachedPolicies,
			&iam.AttachedPolicy{PolicyName: aws.String(name)})
	}
	return out, nil
}

func (s *principalIAMStub) ListAccessKeys(_ string, _ *iam.ListAccessKeysInput) (*iam.ListAccessKeysOutput, error) {
	if s.listKeysErr != nil {
		return nil, s.listKeysErr
	}
	out := &iam.ListAccessKeysOutput{}
	for _, id := range s.accessKeys {
		out.AccessKeyMetadata = append(out.AccessKeyMetadata,
			&iam.AccessKeyMetadata{AccessKeyId: aws.String(id)})
	}
	return out, nil
}

func (s *principalIAMStub) DeleteAccessKey(_ string, input *iam.DeleteAccessKeyInput) (*iam.DeleteAccessKeyOutput, error) {
	s.deletedKeys = append(s.deletedKeys, aws.StringValue(input.AccessKeyId))
	return &iam.DeleteAccessKeyOutput{}, nil
}

// Re-creating a principal with fewer grants has to narrow it. A policy left
// from an earlier name would keep authorising what the operator just removed.
func TestCreateDropsAnInlinePolicyItDidNotWrite(t *testing.T) {
	svc := &principalIAMStub{inlinePolicies: []string{
		"operator-admin-methods", cmd.AdminPrincipalPolicyName, "signup-createaccount",
	}}

	dropped, err := cmd.DropStalePrincipalPolicies(svc, superAdminAccountID, "operator")
	require.NoError(t, err)

	assert.Equal(t, []string{"operator-admin-methods", "signup-createaccount"}, dropped)
	assert.NotContains(t, svc.deletedPolicies, cmd.AdminPrincipalPolicyName,
		"the policy the command owns must survive its own rewrite")
}

// Every key goes, not just the first: two live keys means two holders, and a
// rotation that leaves one behind has revoked nothing.
func TestRevokeRemovesEveryAccessKey(t *testing.T) {
	svc := &principalIAMStub{accessKeys: []string{"AKIAONE", "AKIATWO"}}

	revoked, err := cmd.RevokePrincipalKeys(svc, superAdminAccountID, "operator")
	require.NoError(t, err)

	assert.Equal(t, []string{"AKIAONE", "AKIATWO"}, revoked)
	assert.Equal(t, []string{"AKIAONE", "AKIATWO"}, svc.deletedKeys)
}

// A listing failure must stop the rotation. Minting a second key while an
// unknown number are live is the opposite of what create promises.
func TestRevokeFailsRatherThanGuessing(t *testing.T) {
	svc := &principalIAMStub{listKeysErr: errors.New("kv unavailable")}

	_, err := cmd.RevokePrincipalKeys(svc, superAdminAccountID, "operator")

	require.Error(t, err)
	assert.Empty(t, svc.deletedKeys)
}

// The listing is how an operator answers "who can delete an account", so it
// has to report the scoped grants by method name.
func TestDescribePrincipalReportsScopedGrants(t *testing.T) {
	svc := &principalIAMStub{
		inlineDocument: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Action":["spinifex:ListAccounts","spinifex:CreateAccount"],"Resource":"*"}]}`,
		accessKeys: []string{"AKIAONE"},
	}

	row, err := cmd.DescribePrincipal(svc, superAdminAccountID, "signup")
	require.NoError(t, err)

	assert.Equal(t, "signup", row.UserName)
	assert.Equal(t, []string{"CreateAccount", "ListAccounts"}, row.Grants)
	assert.Equal(t, 1, row.Keys)
}

// An attached wildcard policy is reported rather than expanded: an unscoped
// principal is exactly what the listing exists to surface.
func TestDescribePrincipalMarksAnAttachedPolicy(t *testing.T) {
	svc := &principalIAMStub{attachedPolicies: []string{handlers_iam.AdminPolicyName}}

	row, err := cmd.DescribePrincipal(svc, superAdminAccountID, handlers_iam.AdminUserName)
	require.NoError(t, err)

	assert.Equal(t, []string{handlers_iam.AdminPolicyName + " (attached)"}, row.Grants)
	assert.Zero(t, row.Keys)
}
