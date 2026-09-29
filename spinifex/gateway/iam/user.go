package gateway_iam

import (
	"errors"

	"github.com/aws/aws-sdk-go/aws"
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
	if err := validatePathPrefix(input.PathPrefix, identityPathPrefix); err != nil {
		return nil, err
	}
	p, err := newPager(input.Marker, input.MaxItems)
	if err != nil {
		return nil, err
	}
	out, err := svc.ListUsers(accountID, input)
	if err != nil {
		return nil, err
	}
	out.Users, out.Marker = paginate(p, out.Users, userKey)
	out.IsTruncated = aws.Bool(out.Marker != nil)
	return out, nil
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
	out, err := svc.GetUserPolicy(accountID, input)
	if err != nil {
		return nil, err
	}
	out.PolicyDocument = encodePolicyDocument(out.PolicyDocument)
	return out, nil
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
	p, err := newPager(input.Marker, input.MaxItems)
	if err != nil {
		return nil, err
	}
	out, err := svc.ListUserPolicies(accountID, input)
	if err != nil {
		return nil, err
	}
	out.PolicyNames, out.Marker = paginate(p, out.PolicyNames, aws.StringValue)
	out.IsTruncated = aws.Bool(out.Marker != nil)
	return out, nil
}

func TagUser(accountID string, input *iam.TagUserInput, svc handlers_iam.IAMService) (*iam.TagUserOutput, error) {
	if input.UserName == nil || *input.UserName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if len(input.Tags) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.TagUser(accountID, input)
}

func UntagUser(accountID string, input *iam.UntagUserInput, svc handlers_iam.IAMService) (*iam.UntagUserOutput, error) {
	if input.UserName == nil || *input.UserName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if len(input.TagKeys) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.UntagUser(accountID, input)
}

func ListUserTags(accountID string, input *iam.ListUserTagsInput, svc handlers_iam.IAMService) (*iam.ListUserTagsOutput, error) {
	if input.UserName == nil || *input.UserName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	p, err := newPager(input.Marker, input.MaxItems)
	if err != nil {
		return nil, err
	}
	out, err := svc.ListUserTags(accountID, input)
	if err != nil {
		return nil, err
	}
	out.Tags, out.Marker = paginate(p, out.Tags, tagKey)
	out.IsTruncated = aws.Bool(out.Marker != nil)
	return out, nil
}
