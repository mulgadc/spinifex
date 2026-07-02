package gateway_iam

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
)

func CreateGroup(accountID string, input *iam.CreateGroupInput, svc handlers_iam.IAMService) (*iam.CreateGroupOutput, error) {
	if input.GroupName == nil || *input.GroupName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.CreateGroup(accountID, input)
}

func GetGroup(accountID string, input *iam.GetGroupInput, svc handlers_iam.IAMService) (*iam.GetGroupOutput, error) {
	if input.GroupName == nil || *input.GroupName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.GetGroup(accountID, input)
}

func ListGroups(accountID string, input *iam.ListGroupsInput, svc handlers_iam.IAMService) (*iam.ListGroupsOutput, error) {
	return svc.ListGroups(accountID, input)
}

func DeleteGroup(accountID string, input *iam.DeleteGroupInput, svc handlers_iam.IAMService) (*iam.DeleteGroupOutput, error) {
	if input.GroupName == nil || *input.GroupName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.DeleteGroup(accountID, input)
}

func AddUserToGroup(accountID string, input *iam.AddUserToGroupInput, svc handlers_iam.IAMService) (*iam.AddUserToGroupOutput, error) {
	if input.GroupName == nil || *input.GroupName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.UserName == nil || *input.UserName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.AddUserToGroup(accountID, input)
}

func RemoveUserFromGroup(accountID string, input *iam.RemoveUserFromGroupInput, svc handlers_iam.IAMService) (*iam.RemoveUserFromGroupOutput, error) {
	if input.GroupName == nil || *input.GroupName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.UserName == nil || *input.UserName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.RemoveUserFromGroup(accountID, input)
}

func ListGroupsForUser(accountID string, input *iam.ListGroupsForUserInput, svc handlers_iam.IAMService) (*iam.ListGroupsForUserOutput, error) {
	if input.UserName == nil || *input.UserName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.ListGroupsForUser(accountID, input)
}

func AttachGroupPolicy(accountID string, input *iam.AttachGroupPolicyInput, svc handlers_iam.IAMService) (*iam.AttachGroupPolicyOutput, error) {
	if input.GroupName == nil || *input.GroupName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.PolicyArn == nil || *input.PolicyArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.AttachGroupPolicy(accountID, input)
}

func DetachGroupPolicy(accountID string, input *iam.DetachGroupPolicyInput, svc handlers_iam.IAMService) (*iam.DetachGroupPolicyOutput, error) {
	if input.GroupName == nil || *input.GroupName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.PolicyArn == nil || *input.PolicyArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.DetachGroupPolicy(accountID, input)
}

func ListAttachedGroupPolicies(accountID string, input *iam.ListAttachedGroupPoliciesInput, svc handlers_iam.IAMService) (*iam.ListAttachedGroupPoliciesOutput, error) {
	if input.GroupName == nil || *input.GroupName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.ListAttachedGroupPolicies(accountID, input)
}

func PutGroupPolicy(accountID string, input *iam.PutGroupPolicyInput, svc handlers_iam.IAMService) (*iam.PutGroupPolicyOutput, error) {
	if input.GroupName == nil || *input.GroupName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.PolicyName == nil || *input.PolicyName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.PolicyDocument == nil || *input.PolicyDocument == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.PutGroupPolicy(accountID, input)
}

func GetGroupPolicy(accountID string, input *iam.GetGroupPolicyInput, svc handlers_iam.IAMService) (*iam.GetGroupPolicyOutput, error) {
	if input.GroupName == nil || *input.GroupName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.PolicyName == nil || *input.PolicyName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.GetGroupPolicy(accountID, input)
}

func DeleteGroupPolicy(accountID string, input *iam.DeleteGroupPolicyInput, svc handlers_iam.IAMService) (*iam.DeleteGroupPolicyOutput, error) {
	if input.GroupName == nil || *input.GroupName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.PolicyName == nil || *input.PolicyName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.DeleteGroupPolicy(accountID, input)
}

func ListGroupPolicies(accountID string, input *iam.ListGroupPoliciesInput, svc handlers_iam.IAMService) (*iam.ListGroupPoliciesOutput, error) {
	if input.GroupName == nil || *input.GroupName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.ListGroupPolicies(accountID, input)
}
