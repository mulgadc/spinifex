package gateway_iam

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
)

func CreatePolicy(accountID string, input *iam.CreatePolicyInput, svc handlers_iam.IAMService) (*iam.CreatePolicyOutput, error) {
	if input.PolicyName == nil || *input.PolicyName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.PolicyDocument == nil || *input.PolicyDocument == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.CreatePolicy(accountID, input)
}

func GetPolicy(accountID string, input *iam.GetPolicyInput, svc handlers_iam.IAMService) (*iam.GetPolicyOutput, error) {
	if input.PolicyArn == nil || *input.PolicyArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.GetPolicy(accountID, input)
}

func GetPolicyVersion(accountID string, input *iam.GetPolicyVersionInput, svc handlers_iam.IAMService) (*iam.GetPolicyVersionOutput, error) {
	if input.PolicyArn == nil || *input.PolicyArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.VersionId == nil || *input.VersionId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.GetPolicyVersion(accountID, input)
}

func ListPolicyVersions(accountID string, input *iam.ListPolicyVersionsInput, svc handlers_iam.IAMService) (*iam.ListPolicyVersionsOutput, error) {
	if input.PolicyArn == nil || *input.PolicyArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.ListPolicyVersions(accountID, input)
}

func ListPolicies(accountID string, input *iam.ListPoliciesInput, svc handlers_iam.IAMService) (*iam.ListPoliciesOutput, error) {
	return svc.ListPolicies(accountID, input)
}

func DeletePolicy(accountID string, input *iam.DeletePolicyInput, svc handlers_iam.IAMService) (*iam.DeletePolicyOutput, error) {
	if input.PolicyArn == nil || *input.PolicyArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.DeletePolicy(accountID, input)
}

func AttachUserPolicy(accountID string, input *iam.AttachUserPolicyInput, svc handlers_iam.IAMService) (*iam.AttachUserPolicyOutput, error) {
	if input.UserName == nil || *input.UserName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.PolicyArn == nil || *input.PolicyArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.AttachUserPolicy(accountID, input)
}

func DetachUserPolicy(accountID string, input *iam.DetachUserPolicyInput, svc handlers_iam.IAMService) (*iam.DetachUserPolicyOutput, error) {
	if input.UserName == nil || *input.UserName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.PolicyArn == nil || *input.PolicyArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.DetachUserPolicy(accountID, input)
}

func ListAttachedUserPolicies(accountID string, input *iam.ListAttachedUserPoliciesInput, svc handlers_iam.IAMService) (*iam.ListAttachedUserPoliciesOutput, error) {
	if input.UserName == nil || *input.UserName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.ListAttachedUserPolicies(accountID, input)
}
