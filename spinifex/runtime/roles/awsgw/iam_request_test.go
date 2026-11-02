package awsgw_test

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/iam"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// flexMockIAMService is a configurable mock with per-method overrides. It
// embeds the interface so it satisfies the full contract; only the fn-field
// methods below are wired, and any other method nil-panics if a test reaches
// an unmocked path.
type flexMockIAMService struct {
	iamdomain.IAMService

	createUserFn           func(string, *iam.CreateUserInput) (*iam.CreateUserOutput, error)
	getUserFn              func(string, *iam.GetUserInput) (*iam.GetUserOutput, error)
	listUsersFn            func(string, *iam.ListUsersInput) (*iam.ListUsersOutput, error)
	deleteUserFn           func(string, *iam.DeleteUserInput) (*iam.DeleteUserOutput, error)
	createAccessKeyFn      func(string, *iam.CreateAccessKeyInput) (*iam.CreateAccessKeyOutput, error)
	listAccessKeysFn       func(string, *iam.ListAccessKeysInput) (*iam.ListAccessKeysOutput, error)
	deleteAccessKeyFn      func(string, *iam.DeleteAccessKeyInput) (*iam.DeleteAccessKeyOutput, error)
	updateAccessKeyFn      func(string, *iam.UpdateAccessKeyInput) (*iam.UpdateAccessKeyOutput, error)
	canonicalResourceARNFn func(string, arn.IAMResourceType, string) (string, error)
	lookupAccessKeyFn      func(string) (*iamdomain.AccessKey, error)

	getAccountSummaryFn func(string, *iam.GetAccountSummaryInput) (*iam.GetAccountSummaryOutput, error)
	getRoleFn           func(string, *iam.GetRoleInput) (*iam.GetRoleOutput, error)
}

// GetRole answers NoSuchEntity by default: the STS identity gate resolves the
// target role, and a mock that invented one would let a test pass against a
// role the account does not hold.
func (m *flexMockIAMService) GetRole(accountID string, input *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
	if m.getRoleFn != nil {
		return m.getRoleFn(accountID, input)
	}
	return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
}

func (m *flexMockIAMService) CreateUser(accountID string, input *iam.CreateUserInput) (*iam.CreateUserOutput, error) {
	if m.createUserFn != nil {
		return m.createUserFn(accountID, input)
	}
	return &iam.CreateUserOutput{}, nil
}

func (m *flexMockIAMService) GetUser(accountID string, input *iam.GetUserInput) (*iam.GetUserOutput, error) {
	if m.getUserFn != nil {
		return m.getUserFn(accountID, input)
	}
	return &iam.GetUserOutput{}, nil
}

func (m *flexMockIAMService) ListUsers(accountID string, input *iam.ListUsersInput) (*iam.ListUsersOutput, error) {
	if m.listUsersFn != nil {
		return m.listUsersFn(accountID, input)
	}
	return &iam.ListUsersOutput{}, nil
}

func (m *flexMockIAMService) DeleteUser(accountID string, input *iam.DeleteUserInput) (*iam.DeleteUserOutput, error) {
	if m.deleteUserFn != nil {
		return m.deleteUserFn(accountID, input)
	}
	return &iam.DeleteUserOutput{}, nil
}

func (m *flexMockIAMService) CreateAccessKey(accountID string, input *iam.CreateAccessKeyInput) (*iam.CreateAccessKeyOutput, error) {
	if m.createAccessKeyFn != nil {
		return m.createAccessKeyFn(accountID, input)
	}
	return &iam.CreateAccessKeyOutput{}, nil
}

func (m *flexMockIAMService) ListAccessKeys(accountID string, input *iam.ListAccessKeysInput) (*iam.ListAccessKeysOutput, error) {
	if m.listAccessKeysFn != nil {
		return m.listAccessKeysFn(accountID, input)
	}
	return &iam.ListAccessKeysOutput{}, nil
}

func (m *flexMockIAMService) DeleteAccessKey(accountID string, input *iam.DeleteAccessKeyInput) (*iam.DeleteAccessKeyOutput, error) {
	if m.deleteAccessKeyFn != nil {
		return m.deleteAccessKeyFn(accountID, input)
	}
	return &iam.DeleteAccessKeyOutput{}, nil
}

func (m *flexMockIAMService) UpdateAccessKey(accountID string, input *iam.UpdateAccessKeyInput) (*iam.UpdateAccessKeyOutput, error) {
	if m.updateAccessKeyFn != nil {
		return m.updateAccessKeyFn(accountID, input)
	}
	return &iam.UpdateAccessKeyOutput{}, nil
}

func (m *flexMockIAMService) CanonicalResourceARN(accountID string, kind arn.IAMResourceType, name string) (string, error) {
	if m.canonicalResourceARNFn != nil {
		return m.canonicalResourceARNFn(accountID, kind, name)
	}
	return arn.FormatIAMPath(kind, accountID, "/", name), nil
}

func (m *flexMockIAMService) LookupAccessKey(accessKeyID string) (*iamdomain.AccessKey, error) {
	if m.lookupAccessKeyFn != nil {
		return m.lookupAccessKeyFn(accessKeyID)
	}
	return &iamdomain.AccessKey{AccessKeyID: accessKeyID, AccountID: "000000000000", UserName: "user"}, nil
}

func (m *flexMockIAMService) GetAccountSummary(accountID string, input *iam.GetAccountSummaryInput) (*iam.GetAccountSummaryOutput, error) {
	if m.getAccountSummaryFn != nil {
		return m.getAccountSummaryFn(accountID, input)
	}
	return &iam.GetAccountSummaryOutput{}, nil
}
func (m *flexMockIAMService) ListAccountAliases(_ string, _ *iam.ListAccountAliasesInput) (*iam.ListAccountAliasesOutput, error) {
	return &iam.ListAccountAliasesOutput{}, nil
}
func (m *flexMockIAMService) CreateGroup(_ string, _ *iam.CreateGroupInput) (*iam.CreateGroupOutput, error) {
	return &iam.CreateGroupOutput{}, nil
}
func (m *flexMockIAMService) GetGroup(_ string, _ *iam.GetGroupInput) (*iam.GetGroupOutput, error) {
	return &iam.GetGroupOutput{}, nil
}
func (m *flexMockIAMService) ListGroups(_ string, _ *iam.ListGroupsInput) (*iam.ListGroupsOutput, error) {
	return &iam.ListGroupsOutput{}, nil
}
func (m *flexMockIAMService) DeleteGroup(_ string, _ *iam.DeleteGroupInput) (*iam.DeleteGroupOutput, error) {
	return &iam.DeleteGroupOutput{}, nil
}
func (m *flexMockIAMService) AddUserToGroup(_ string, _ *iam.AddUserToGroupInput) (*iam.AddUserToGroupOutput, error) {
	return &iam.AddUserToGroupOutput{}, nil
}
func (m *flexMockIAMService) RemoveUserFromGroup(_ string, _ *iam.RemoveUserFromGroupInput) (*iam.RemoveUserFromGroupOutput, error) {
	return &iam.RemoveUserFromGroupOutput{}, nil
}
func (m *flexMockIAMService) ListGroupsForUser(_ string, _ *iam.ListGroupsForUserInput) (*iam.ListGroupsForUserOutput, error) {
	return &iam.ListGroupsForUserOutput{}, nil
}
func (m *flexMockIAMService) AttachGroupPolicy(_ string, _ *iam.AttachGroupPolicyInput) (*iam.AttachGroupPolicyOutput, error) {
	return &iam.AttachGroupPolicyOutput{}, nil
}
func (m *flexMockIAMService) DetachGroupPolicy(_ string, _ *iam.DetachGroupPolicyInput) (*iam.DetachGroupPolicyOutput, error) {
	return &iam.DetachGroupPolicyOutput{}, nil
}
func (m *flexMockIAMService) ListAttachedGroupPolicies(_ string, _ *iam.ListAttachedGroupPoliciesInput) (*iam.ListAttachedGroupPoliciesOutput, error) {
	return &iam.ListAttachedGroupPoliciesOutput{}, nil
}
func (m *flexMockIAMService) PutGroupPolicy(_ string, _ *iam.PutGroupPolicyInput) (*iam.PutGroupPolicyOutput, error) {
	return &iam.PutGroupPolicyOutput{}, nil
}
func (m *flexMockIAMService) GetGroupPolicy(_ string, _ *iam.GetGroupPolicyInput) (*iam.GetGroupPolicyOutput, error) {
	return &iam.GetGroupPolicyOutput{}, nil
}
func (m *flexMockIAMService) DeleteGroupPolicy(_ string, _ *iam.DeleteGroupPolicyInput) (*iam.DeleteGroupPolicyOutput, error) {
	return &iam.DeleteGroupPolicyOutput{}, nil
}
func (m *flexMockIAMService) ListGroupPolicies(_ string, _ *iam.ListGroupPoliciesInput) (*iam.ListGroupPoliciesOutput, error) {
	return &iam.ListGroupPoliciesOutput{}, nil
}
func (m *flexMockIAMService) PutUserPolicy(_ string, _ *iam.PutUserPolicyInput) (*iam.PutUserPolicyOutput, error) {
	return &iam.PutUserPolicyOutput{}, nil
}
func (m *flexMockIAMService) GetUserPolicy(_ string, _ *iam.GetUserPolicyInput) (*iam.GetUserPolicyOutput, error) {
	return &iam.GetUserPolicyOutput{}, nil
}
func (m *flexMockIAMService) DeleteUserPolicy(_ string, _ *iam.DeleteUserPolicyInput) (*iam.DeleteUserPolicyOutput, error) {
	return &iam.DeleteUserPolicyOutput{}, nil
}
func (m *flexMockIAMService) ListUserPolicies(_ string, _ *iam.ListUserPoliciesInput) (*iam.ListUserPoliciesOutput, error) {
	return &iam.ListUserPoliciesOutput{}, nil
}
