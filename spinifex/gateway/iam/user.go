package gateway_iam

import (
	"errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
)

// CreateUser implements the IAM CreateUser action, creating an IAM user. It returns
// MissingParameter when UserName is absent, then calls the IAM service for accountID.
func CreateUser(accountID string, input *iam.CreateUserInput, svc handlers_iam.IAMService) (*iam.CreateUserOutput, error) {
	if input.UserName == nil || *input.UserName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.CreateUser(accountID, input)
}

// GetUser implements the IAM GetUser action. UserName is required (MissingParameter); the AWS
// default of describing the calling user is not supported.
func GetUser(accountID string, input *iam.GetUserInput, svc handlers_iam.IAMService) (*iam.GetUserOutput, error) {
	if input.UserName == nil || *input.UserName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.GetUser(accountID, input)
}

// ListUsers implements the IAM ListUsers action, listing the account's users. It validates
// PathPrefix; results are paged here by Marker and MaxItems.
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

// DeleteUser implements the IAM DeleteUser action, deleting an IAM user. It returns
// MissingParameter when UserName is absent, then calls the IAM service for accountID.
func DeleteUser(accountID string, input *iam.DeleteUserInput, svc handlers_iam.IAMService) (*iam.DeleteUserOutput, error) {
	if input.UserName == nil || *input.UserName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.DeleteUser(accountID, input)
}

// PutUserPolicy implements the IAM PutUserPolicy action, adding or replacing a user inline
// policy. It returns MissingParameter when UserName, PolicyName or PolicyDocument is absent, then
// calls the IAM service for accountID.
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

// GetUserPolicy implements the IAM GetUserPolicy action, reading a user inline policy. It
// requires UserName or PolicyName and returns the document percent-encoded as the IAM Query API
// does.
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

// DeleteUserPolicy implements the IAM DeleteUserPolicy action, deleting a user inline policy. It
// returns MissingParameter when UserName or PolicyName is absent, then calls the IAM service for
// accountID.
func DeleteUserPolicy(accountID string, input *iam.DeleteUserPolicyInput, svc handlers_iam.IAMService) (*iam.DeleteUserPolicyOutput, error) {
	if input.UserName == nil || *input.UserName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.PolicyName == nil || *input.PolicyName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.DeleteUserPolicy(accountID, input)
}

// ListUserPolicies implements the IAM ListUserPolicies action, listing a user's inline policy
// names. It requires UserName; results are paged here by Marker and MaxItems.
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

// TagUser implements the IAM TagUser action. It returns MissingParameter when UserName is absent
// or Tags is empty.
func TagUser(accountID string, input *iam.TagUserInput, svc handlers_iam.IAMService) (*iam.TagUserOutput, error) {
	if input.UserName == nil || *input.UserName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if len(input.Tags) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.TagUser(accountID, input)
}

// UntagUser implements the IAM UntagUser action. It returns MissingParameter when UserName is
// absent or TagKeys is empty.
func UntagUser(accountID string, input *iam.UntagUserInput, svc handlers_iam.IAMService) (*iam.UntagUserOutput, error) {
	if input.UserName == nil || *input.UserName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if len(input.TagKeys) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.UntagUser(accountID, input)
}

// ListUserTags implements the IAM ListUserTags action, listing a user's tags. It requires
// UserName; results are paged here by Marker and MaxItems.
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
