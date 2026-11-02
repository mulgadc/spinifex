package awsgw_test

import (
	"fmt"

	"github.com/aws/aws-sdk-go/service/iam"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
)

type echoIAMService struct {
	flexMockIAMService

	wantOutput   any
	gotAccountID string
	gotInput     any
}

func echoIAMCall[In any, Out any](svc *echoIAMService, accountID string, input *In) (*Out, error) {
	svc.gotAccountID = accountID
	svc.gotInput = input

	if svc.wantOutput == nil {
		return new(Out), nil
	}
	output, ok := svc.wantOutput.(*Out)
	if !ok {
		return nil, fmt.Errorf("echo IAM output type %T is not %T", svc.wantOutput, new(Out))
	}
	return output, nil
}

func (svc *echoIAMService) AddRoleToInstanceProfile(accountID string, input *iam.AddRoleToInstanceProfileInput) (*iam.AddRoleToInstanceProfileOutput, error) {
	return echoIAMCall[iam.AddRoleToInstanceProfileInput, iam.AddRoleToInstanceProfileOutput](svc, accountID, input)
}

func (svc *echoIAMService) AddUserToGroup(accountID string, input *iam.AddUserToGroupInput) (*iam.AddUserToGroupOutput, error) {
	return echoIAMCall[iam.AddUserToGroupInput, iam.AddUserToGroupOutput](svc, accountID, input)
}

func (svc *echoIAMService) AttachGroupPolicy(accountID string, input *iam.AttachGroupPolicyInput) (*iam.AttachGroupPolicyOutput, error) {
	return echoIAMCall[iam.AttachGroupPolicyInput, iam.AttachGroupPolicyOutput](svc, accountID, input)
}

func (svc *echoIAMService) AttachRolePolicy(accountID string, input *iam.AttachRolePolicyInput) (*iam.AttachRolePolicyOutput, error) {
	return echoIAMCall[iam.AttachRolePolicyInput, iam.AttachRolePolicyOutput](svc, accountID, input)
}

func (svc *echoIAMService) AttachUserPolicy(accountID string, input *iam.AttachUserPolicyInput) (*iam.AttachUserPolicyOutput, error) {
	return echoIAMCall[iam.AttachUserPolicyInput, iam.AttachUserPolicyOutput](svc, accountID, input)
}

func (svc *echoIAMService) CreateAccessKey(accountID string, input *iam.CreateAccessKeyInput) (*iam.CreateAccessKeyOutput, error) {
	return echoIAMCall[iam.CreateAccessKeyInput, iam.CreateAccessKeyOutput](svc, accountID, input)
}

func (svc *echoIAMService) CreateGroup(accountID string, input *iam.CreateGroupInput) (*iam.CreateGroupOutput, error) {
	return echoIAMCall[iam.CreateGroupInput, iam.CreateGroupOutput](svc, accountID, input)
}

func (svc *echoIAMService) CreateInstanceProfile(accountID string, input *iam.CreateInstanceProfileInput) (*iam.CreateInstanceProfileOutput, error) {
	return echoIAMCall[iam.CreateInstanceProfileInput, iam.CreateInstanceProfileOutput](svc, accountID, input)
}

func (svc *echoIAMService) CreateOpenIDConnectProvider(accountID string, input *iam.CreateOpenIDConnectProviderInput) (*iam.CreateOpenIDConnectProviderOutput, error) {
	return echoIAMCall[iam.CreateOpenIDConnectProviderInput, iam.CreateOpenIDConnectProviderOutput](svc, accountID, input)
}

func (svc *echoIAMService) CreateRole(accountID string, input *iam.CreateRoleInput) (*iam.CreateRoleOutput, error) {
	return echoIAMCall[iam.CreateRoleInput, iam.CreateRoleOutput](svc, accountID, input)
}

func (svc *echoIAMService) CreateUser(accountID string, input *iam.CreateUserInput) (*iam.CreateUserOutput, error) {
	return echoIAMCall[iam.CreateUserInput, iam.CreateUserOutput](svc, accountID, input)
}

func (svc *echoIAMService) DeleteAccessKey(accountID string, input *iam.DeleteAccessKeyInput) (*iam.DeleteAccessKeyOutput, error) {
	return echoIAMCall[iam.DeleteAccessKeyInput, iam.DeleteAccessKeyOutput](svc, accountID, input)
}

func (svc *echoIAMService) DeleteGroupPolicy(accountID string, input *iam.DeleteGroupPolicyInput) (*iam.DeleteGroupPolicyOutput, error) {
	return echoIAMCall[iam.DeleteGroupPolicyInput, iam.DeleteGroupPolicyOutput](svc, accountID, input)
}

func (svc *echoIAMService) DeleteInstanceProfile(accountID string, input *iam.DeleteInstanceProfileInput) (*iam.DeleteInstanceProfileOutput, error) {
	return echoIAMCall[iam.DeleteInstanceProfileInput, iam.DeleteInstanceProfileOutput](svc, accountID, input)
}

func (svc *echoIAMService) DeleteRole(accountID string, input *iam.DeleteRoleInput) (*iam.DeleteRoleOutput, error) {
	return echoIAMCall[iam.DeleteRoleInput, iam.DeleteRoleOutput](svc, accountID, input)
}

func (svc *echoIAMService) DeleteRolePolicy(accountID string, input *iam.DeleteRolePolicyInput) (*iam.DeleteRolePolicyOutput, error) {
	return echoIAMCall[iam.DeleteRolePolicyInput, iam.DeleteRolePolicyOutput](svc, accountID, input)
}

func (svc *echoIAMService) DeleteUser(accountID string, input *iam.DeleteUserInput) (*iam.DeleteUserOutput, error) {
	return echoIAMCall[iam.DeleteUserInput, iam.DeleteUserOutput](svc, accountID, input)
}

func (svc *echoIAMService) DeleteUserPolicy(accountID string, input *iam.DeleteUserPolicyInput) (*iam.DeleteUserPolicyOutput, error) {
	return echoIAMCall[iam.DeleteUserPolicyInput, iam.DeleteUserPolicyOutput](svc, accountID, input)
}

func (svc *echoIAMService) GetAccountSummary(accountID string, input *iam.GetAccountSummaryInput) (*iam.GetAccountSummaryOutput, error) {
	return echoIAMCall[iam.GetAccountSummaryInput, iam.GetAccountSummaryOutput](svc, accountID, input)
}

func (svc *echoIAMService) ListAccountAliases(accountID string, input *iam.ListAccountAliasesInput) (*iam.ListAccountAliasesOutput, error) {
	return echoIAMCall[iam.ListAccountAliasesInput, iam.ListAccountAliasesOutput](svc, accountID, input)
}

func (svc *echoIAMService) GetInstanceProfile(accountID string, input *iam.GetInstanceProfileInput) (*iam.GetInstanceProfileOutput, error) {
	if _, ok := svc.wantOutput.(*iam.GetInstanceProfileOutput); !ok {
		return &iam.GetInstanceProfileOutput{InstanceProfile: &iam.InstanceProfile{}}, nil
	}
	return echoIAMCall[iam.GetInstanceProfileInput, iam.GetInstanceProfileOutput](svc, accountID, input)
}

func (svc *echoIAMService) GetRole(accountID string, input *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
	return echoIAMCall[iam.GetRoleInput, iam.GetRoleOutput](svc, accountID, input)
}

func (svc *echoIAMService) GetUser(accountID string, input *iam.GetUserInput) (*iam.GetUserOutput, error) {
	return echoIAMCall[iam.GetUserInput, iam.GetUserOutput](svc, accountID, input)
}

func (svc *echoIAMService) ListAccessKeys(accountID string, input *iam.ListAccessKeysInput) (*iam.ListAccessKeysOutput, error) {
	return echoIAMCall[iam.ListAccessKeysInput, iam.ListAccessKeysOutput](svc, accountID, input)
}

func (svc *echoIAMService) ListGroupPolicies(accountID string, input *iam.ListGroupPoliciesInput) (*iam.ListGroupPoliciesOutput, error) {
	return echoIAMCall[iam.ListGroupPoliciesInput, iam.ListGroupPoliciesOutput](svc, accountID, input)
}

func (svc *echoIAMService) ListGroups(accountID string, input *iam.ListGroupsInput) (*iam.ListGroupsOutput, error) {
	return echoIAMCall[iam.ListGroupsInput, iam.ListGroupsOutput](svc, accountID, input)
}

func (svc *echoIAMService) ListGroupsForUser(accountID string, input *iam.ListGroupsForUserInput) (*iam.ListGroupsForUserOutput, error) {
	return echoIAMCall[iam.ListGroupsForUserInput, iam.ListGroupsForUserOutput](svc, accountID, input)
}

func (svc *echoIAMService) ListRoleTags(accountID string, input *iam.ListRoleTagsInput) (*iam.ListRoleTagsOutput, error) {
	return echoIAMCall[iam.ListRoleTagsInput, iam.ListRoleTagsOutput](svc, accountID, input)
}

func (svc *echoIAMService) ListUserTags(accountID string, input *iam.ListUserTagsInput) (*iam.ListUserTagsOutput, error) {
	return echoIAMCall[iam.ListUserTagsInput, iam.ListUserTagsOutput](svc, accountID, input)
}

func (svc *echoIAMService) ListUsers(accountID string, input *iam.ListUsersInput) (*iam.ListUsersOutput, error) {
	return echoIAMCall[iam.ListUsersInput, iam.ListUsersOutput](svc, accountID, input)
}

func (svc *echoIAMService) PutGroupPolicy(accountID string, input *iam.PutGroupPolicyInput) (*iam.PutGroupPolicyOutput, error) {
	return echoIAMCall[iam.PutGroupPolicyInput, iam.PutGroupPolicyOutput](svc, accountID, input)
}

func (svc *echoIAMService) PutRolePolicy(accountID string, input *iam.PutRolePolicyInput) (*iam.PutRolePolicyOutput, error) {
	return echoIAMCall[iam.PutRolePolicyInput, iam.PutRolePolicyOutput](svc, accountID, input)
}

func (svc *echoIAMService) PutUserPolicy(accountID string, input *iam.PutUserPolicyInput) (*iam.PutUserPolicyOutput, error) {
	return echoIAMCall[iam.PutUserPolicyInput, iam.PutUserPolicyOutput](svc, accountID, input)
}

func (svc *echoIAMService) RemoveRoleFromInstanceProfile(accountID string, input *iam.RemoveRoleFromInstanceProfileInput) (*iam.RemoveRoleFromInstanceProfileOutput, error) {
	return echoIAMCall[iam.RemoveRoleFromInstanceProfileInput, iam.RemoveRoleFromInstanceProfileOutput](svc, accountID, input)
}

func (svc *echoIAMService) RemoveUserFromGroup(accountID string, input *iam.RemoveUserFromGroupInput) (*iam.RemoveUserFromGroupOutput, error) {
	return echoIAMCall[iam.RemoveUserFromGroupInput, iam.RemoveUserFromGroupOutput](svc, accountID, input)
}

func (svc *echoIAMService) TagRole(accountID string, input *iam.TagRoleInput) (*iam.TagRoleOutput, error) {
	return echoIAMCall[iam.TagRoleInput, iam.TagRoleOutput](svc, accountID, input)
}

func (svc *echoIAMService) TagUser(accountID string, input *iam.TagUserInput) (*iam.TagUserOutput, error) {
	return echoIAMCall[iam.TagUserInput, iam.TagUserOutput](svc, accountID, input)
}

func (svc *echoIAMService) UntagRole(accountID string, input *iam.UntagRoleInput) (*iam.UntagRoleOutput, error) {
	return echoIAMCall[iam.UntagRoleInput, iam.UntagRoleOutput](svc, accountID, input)
}

func (svc *echoIAMService) UntagUser(accountID string, input *iam.UntagUserInput) (*iam.UntagUserOutput, error) {
	return echoIAMCall[iam.UntagUserInput, iam.UntagUserOutput](svc, accountID, input)
}

func (svc *echoIAMService) UpdateAccessKey(accountID string, input *iam.UpdateAccessKeyInput) (*iam.UpdateAccessKeyOutput, error) {
	return echoIAMCall[iam.UpdateAccessKeyInput, iam.UpdateAccessKeyOutput](svc, accountID, input)
}

func (svc *echoIAMService) UpdateAssumeRolePolicy(accountID string, input *iam.UpdateAssumeRolePolicyInput) (*iam.UpdateAssumeRolePolicyOutput, error) {
	return echoIAMCall[iam.UpdateAssumeRolePolicyInput, iam.UpdateAssumeRolePolicyOutput](svc, accountID, input)
}

var _ iamdomain.IAMService = (*echoIAMService)(nil)
