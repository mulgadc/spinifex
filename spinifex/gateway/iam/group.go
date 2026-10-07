package gateway_iam

import (
	"errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
)

// CreateGroup implements the IAM CreateGroup action, creating an IAM group. It returns
// MissingParameter when GroupName is absent, then calls the IAM service for accountID.
func CreateGroup(accountID string, input *iam.CreateGroupInput, svc handlers_iam.IAMService) (*iam.CreateGroupOutput, error) {
	if input.GroupName == nil || *input.GroupName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.CreateGroup(accountID, input)
}

// GetGroup implements the IAM GetGroup action, returning the group and its members. It requires
// GroupName; the Users list is paged here by Marker and MaxItems.
func GetGroup(accountID string, input *iam.GetGroupInput, svc handlers_iam.IAMService) (*iam.GetGroupOutput, error) {
	if input.GroupName == nil || *input.GroupName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	p, err := newPager(input.Marker, input.MaxItems)
	if err != nil {
		return nil, err
	}
	out, err := svc.GetGroup(accountID, input)
	if err != nil {
		return nil, err
	}
	out.Users, out.Marker = paginate(p, out.Users, userKey)
	out.IsTruncated = aws.Bool(out.Marker != nil)
	return out, nil
}

// ListGroups implements the IAM ListGroups action, listing the account's groups. It validates
// PathPrefix; results are paged here by Marker and MaxItems.
func ListGroups(accountID string, input *iam.ListGroupsInput, svc handlers_iam.IAMService) (*iam.ListGroupsOutput, error) {
	if err := validatePathPrefix(input.PathPrefix, identityPathPrefix); err != nil {
		return nil, err
	}
	p, err := newPager(input.Marker, input.MaxItems)
	if err != nil {
		return nil, err
	}
	out, err := svc.ListGroups(accountID, input)
	if err != nil {
		return nil, err
	}
	out.Groups, out.Marker = paginate(p, out.Groups, groupKey)
	out.IsTruncated = aws.Bool(out.Marker != nil)
	return out, nil
}

// DeleteGroup implements the IAM DeleteGroup action, deleting an IAM group. It returns
// MissingParameter when GroupName is absent, then calls the IAM service for accountID.
func DeleteGroup(accountID string, input *iam.DeleteGroupInput, svc handlers_iam.IAMService) (*iam.DeleteGroupOutput, error) {
	if input.GroupName == nil || *input.GroupName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.DeleteGroup(accountID, input)
}

// AddUserToGroup implements the IAM AddUserToGroup action, adding a user to a group. It returns
// MissingParameter when GroupName or UserName is absent, then calls the IAM service for
// accountID.
func AddUserToGroup(accountID string, input *iam.AddUserToGroupInput, svc handlers_iam.IAMService) (*iam.AddUserToGroupOutput, error) {
	if input.GroupName == nil || *input.GroupName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.UserName == nil || *input.UserName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.AddUserToGroup(accountID, input)
}

// RemoveUserFromGroup implements the IAM RemoveUserFromGroup action, removing a user from a
// group. It returns MissingParameter when GroupName or UserName is absent, then calls the IAM
// service for accountID.
func RemoveUserFromGroup(accountID string, input *iam.RemoveUserFromGroupInput, svc handlers_iam.IAMService) (*iam.RemoveUserFromGroupOutput, error) {
	if input.GroupName == nil || *input.GroupName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.UserName == nil || *input.UserName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.RemoveUserFromGroup(accountID, input)
}

// ListGroupsForUser implements the IAM ListGroupsForUser action, listing the groups a user
// belongs to. It requires UserName; results are paged here by Marker and MaxItems.
func ListGroupsForUser(accountID string, input *iam.ListGroupsForUserInput, svc handlers_iam.IAMService) (*iam.ListGroupsForUserOutput, error) {
	if input.UserName == nil || *input.UserName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	p, err := newPager(input.Marker, input.MaxItems)
	if err != nil {
		return nil, err
	}
	out, err := svc.ListGroupsForUser(accountID, input)
	if err != nil {
		return nil, err
	}
	out.Groups, out.Marker = paginate(p, out.Groups, groupKey)
	out.IsTruncated = aws.Bool(out.Marker != nil)
	return out, nil
}

// AttachGroupPolicy implements the IAM AttachGroupPolicy action, attaching a managed policy to a
// group. It returns MissingParameter when GroupName or PolicyArn is absent, then calls the IAM
// service for accountID.
func AttachGroupPolicy(accountID string, input *iam.AttachGroupPolicyInput, svc handlers_iam.IAMService) (*iam.AttachGroupPolicyOutput, error) {
	if input.GroupName == nil || *input.GroupName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.PolicyArn == nil || *input.PolicyArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.AttachGroupPolicy(accountID, input)
}

// DetachGroupPolicy implements the IAM DetachGroupPolicy action, detaching a managed policy from
// a group. It returns MissingParameter when GroupName or PolicyArn is absent, then calls the IAM
// service for accountID.
func DetachGroupPolicy(accountID string, input *iam.DetachGroupPolicyInput, svc handlers_iam.IAMService) (*iam.DetachGroupPolicyOutput, error) {
	if input.GroupName == nil || *input.GroupName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.PolicyArn == nil || *input.PolicyArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.DetachGroupPolicy(accountID, input)
}

// ListAttachedGroupPolicies implements the IAM ListAttachedGroupPolicies action, listing a
// group's managed policies. It requires GroupName and validates PathPrefix; results are paged
// here by Marker and MaxItems.
func ListAttachedGroupPolicies(accountID string, input *iam.ListAttachedGroupPoliciesInput, svc handlers_iam.IAMService) (*iam.ListAttachedGroupPoliciesOutput, error) {
	if input.GroupName == nil || *input.GroupName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if err := validatePathPrefix(input.PathPrefix, policyPathPrefix); err != nil {
		return nil, err
	}
	p, err := newPager(input.Marker, input.MaxItems)
	if err != nil {
		return nil, err
	}
	out, err := svc.ListAttachedGroupPolicies(accountID, input)
	if err != nil {
		return nil, err
	}
	out.AttachedPolicies, out.Marker = paginate(p, out.AttachedPolicies, attachedPolicyKey)
	out.IsTruncated = aws.Bool(out.Marker != nil)
	return out, nil
}

// PutGroupPolicy implements the IAM PutGroupPolicy action, adding or replacing a group inline
// policy. It returns MissingParameter when GroupName, PolicyName or PolicyDocument is absent,
// then calls the IAM service for accountID.
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

// GetGroupPolicy implements the IAM GetGroupPolicy action, reading a group inline policy. It
// requires GroupName or PolicyName and returns the document percent-encoded as the IAM Query API
// does.
func GetGroupPolicy(accountID string, input *iam.GetGroupPolicyInput, svc handlers_iam.IAMService) (*iam.GetGroupPolicyOutput, error) {
	if input.GroupName == nil || *input.GroupName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.PolicyName == nil || *input.PolicyName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	out, err := svc.GetGroupPolicy(accountID, input)
	if err != nil {
		return nil, err
	}
	out.PolicyDocument = encodePolicyDocument(out.PolicyDocument)
	return out, nil
}

// DeleteGroupPolicy implements the IAM DeleteGroupPolicy action, deleting a group inline policy.
// It returns MissingParameter when GroupName or PolicyName is absent, then calls the IAM service
// for accountID.
func DeleteGroupPolicy(accountID string, input *iam.DeleteGroupPolicyInput, svc handlers_iam.IAMService) (*iam.DeleteGroupPolicyOutput, error) {
	if input.GroupName == nil || *input.GroupName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.PolicyName == nil || *input.PolicyName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.DeleteGroupPolicy(accountID, input)
}

// ListGroupPolicies implements the IAM ListGroupPolicies action, listing a group's inline policy
// names. It requires GroupName; results are paged here by Marker and MaxItems.
func ListGroupPolicies(accountID string, input *iam.ListGroupPoliciesInput, svc handlers_iam.IAMService) (*iam.ListGroupPoliciesOutput, error) {
	if input.GroupName == nil || *input.GroupName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	p, err := newPager(input.Marker, input.MaxItems)
	if err != nil {
		return nil, err
	}
	out, err := svc.ListGroupPolicies(accountID, input)
	if err != nil {
		return nil, err
	}
	out.PolicyNames, out.Marker = paginate(p, out.PolicyNames, aws.StringValue)
	out.IsTruncated = aws.Bool(out.Marker != nil)
	return out, nil
}
