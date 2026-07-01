package gateway_iam

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
)

func CreateUser(accountID string, input *iam.CreateUserInput, svc handlers_iam.IAMService) (*iam.CreateUserOutput, error) {
	if input.UserName == nil || *input.UserName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.CreateUser(accountID, input)
}

func GetUser(accountID string, input *iam.GetUserInput, svc handlers_iam.IAMService) (*iam.GetUserOutput, error) {
	if input.UserName == nil || *input.UserName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.GetUser(accountID, input)
}

func ListUsers(accountID string, input *iam.ListUsersInput, svc handlers_iam.IAMService) (*iam.ListUsersOutput, error) {
	return svc.ListUsers(accountID, input)
}

func DeleteUser(accountID string, input *iam.DeleteUserInput, svc handlers_iam.IAMService) (*iam.DeleteUserOutput, error) {
	if input.UserName == nil || *input.UserName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.DeleteUser(accountID, input)
}

func PutUserPolicy(accountID string, input *iam.PutUserPolicyInput, svc handlers_iam.IAMService) (*iam.PutUserPolicyOutput, error) {
	if input.UserName == nil || *input.UserName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.PolicyName == nil || *input.PolicyName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.PolicyDocument == nil || *input.PolicyDocument == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.PutUserPolicy(accountID, input)
}

func GetUserPolicy(accountID string, input *iam.GetUserPolicyInput, svc handlers_iam.IAMService) (*iam.GetUserPolicyOutput, error) {
	if input.UserName == nil || *input.UserName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.PolicyName == nil || *input.PolicyName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.GetUserPolicy(accountID, input)
}

func DeleteUserPolicy(accountID string, input *iam.DeleteUserPolicyInput, svc handlers_iam.IAMService) (*iam.DeleteUserPolicyOutput, error) {
	if input.UserName == nil || *input.UserName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.PolicyName == nil || *input.PolicyName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.DeleteUserPolicy(accountID, input)
}

func ListUserPolicies(accountID string, input *iam.ListUserPoliciesInput, svc handlers_iam.IAMService) (*iam.ListUserPoliciesOutput, error) {
	if input.UserName == nil || *input.UserName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.ListUserPolicies(accountID, input)
}
