package gateway_iam

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
)

func CreateRole(accountID string, input *iam.CreateRoleInput, svc handlers_iam.IAMService) (*iam.CreateRoleOutput, error) {
	if input.RoleName == nil || *input.RoleName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.AssumeRolePolicyDocument == nil || *input.AssumeRolePolicyDocument == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.CreateRole(accountID, input)
}

func GetRole(accountID string, input *iam.GetRoleInput, svc handlers_iam.IAMService) (*iam.GetRoleOutput, error) {
	if input.RoleName == nil || *input.RoleName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.GetRole(accountID, input)
}

func ListRoles(accountID string, input *iam.ListRolesInput, svc handlers_iam.IAMService) (*iam.ListRolesOutput, error) {
	return svc.ListRoles(accountID, input)
}

func DeleteRole(accountID string, input *iam.DeleteRoleInput, svc handlers_iam.IAMService) (*iam.DeleteRoleOutput, error) {
	if input.RoleName == nil || *input.RoleName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.DeleteRole(accountID, input)
}

func UpdateRole(accountID string, input *iam.UpdateRoleInput, svc handlers_iam.IAMService) (*iam.UpdateRoleOutput, error) {
	if input.RoleName == nil || *input.RoleName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.UpdateRole(accountID, input)
}

func UpdateAssumeRolePolicy(accountID string, input *iam.UpdateAssumeRolePolicyInput, svc handlers_iam.IAMService) (*iam.UpdateAssumeRolePolicyOutput, error) {
	if input.RoleName == nil || *input.RoleName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.PolicyDocument == nil || *input.PolicyDocument == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.UpdateAssumeRolePolicy(accountID, input)
}

func AttachRolePolicy(accountID string, input *iam.AttachRolePolicyInput, svc handlers_iam.IAMService) (*iam.AttachRolePolicyOutput, error) {
	if input.RoleName == nil || *input.RoleName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.PolicyArn == nil || *input.PolicyArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.AttachRolePolicy(accountID, input)
}

func DetachRolePolicy(accountID string, input *iam.DetachRolePolicyInput, svc handlers_iam.IAMService) (*iam.DetachRolePolicyOutput, error) {
	if input.RoleName == nil || *input.RoleName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.PolicyArn == nil || *input.PolicyArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.DetachRolePolicy(accountID, input)
}

func ListAttachedRolePolicies(accountID string, input *iam.ListAttachedRolePoliciesInput, svc handlers_iam.IAMService) (*iam.ListAttachedRolePoliciesOutput, error) {
	if input.RoleName == nil || *input.RoleName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.ListAttachedRolePolicies(accountID, input)
}

func PutRolePolicy(accountID string, input *iam.PutRolePolicyInput, svc handlers_iam.IAMService) (*iam.PutRolePolicyOutput, error) {
	if input.RoleName == nil || *input.RoleName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.PolicyName == nil || *input.PolicyName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.PolicyDocument == nil || *input.PolicyDocument == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.PutRolePolicy(accountID, input)
}

func GetRolePolicy(accountID string, input *iam.GetRolePolicyInput, svc handlers_iam.IAMService) (*iam.GetRolePolicyOutput, error) {
	if input.RoleName == nil || *input.RoleName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.PolicyName == nil || *input.PolicyName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.GetRolePolicy(accountID, input)
}

func DeleteRolePolicy(accountID string, input *iam.DeleteRolePolicyInput, svc handlers_iam.IAMService) (*iam.DeleteRolePolicyOutput, error) {
	if input.RoleName == nil || *input.RoleName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.PolicyName == nil || *input.PolicyName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.DeleteRolePolicy(accountID, input)
}

func ListRolePolicies(accountID string, input *iam.ListRolePoliciesInput, svc handlers_iam.IAMService) (*iam.ListRolePoliciesOutput, error) {
	if input.RoleName == nil || *input.RoleName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.ListRolePolicies(accountID, input)
}
