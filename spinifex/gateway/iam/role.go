package gateway_iam

import (
	"errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
)

// CreateRole implements the IAM CreateRole action. It requires RoleName and
// AssumeRolePolicyDocument and returns the trust policy percent-encoded as the IAM Query API
// does.
func CreateRole(accountID string, input *iam.CreateRoleInput, svc handlers_iam.IAMService) (*iam.CreateRoleOutput, error) {
	if input.RoleName == nil || *input.RoleName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.AssumeRolePolicyDocument == nil || *input.AssumeRolePolicyDocument == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	out, err := svc.CreateRole(accountID, input)
	if err != nil {
		return nil, err
	}
	encodeRoleDocuments(out.Role)
	return out, nil
}

// GetRole implements the IAM GetRole action. It requires RoleName and returns the trust policy
// percent-encoded as the IAM Query API does.
func GetRole(accountID string, input *iam.GetRoleInput, svc handlers_iam.IAMService) (*iam.GetRoleOutput, error) {
	if input.RoleName == nil || *input.RoleName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	out, err := svc.GetRole(accountID, input)
	if err != nil {
		return nil, err
	}
	encodeRoleDocuments(out.Role)
	return out, nil
}

// ListRoles implements the IAM ListRoles action. It validates PathPrefix, pages results here by
// Marker and MaxItems, and percent-encodes each trust policy.
func ListRoles(accountID string, input *iam.ListRolesInput, svc handlers_iam.IAMService) (*iam.ListRolesOutput, error) {
	if err := validatePathPrefix(input.PathPrefix, identityPathPrefix); err != nil {
		return nil, err
	}
	p, err := newPager(input.Marker, input.MaxItems)
	if err != nil {
		return nil, err
	}
	out, err := svc.ListRoles(accountID, input)
	if err != nil {
		return nil, err
	}
	out.Roles, out.Marker = paginate(p, out.Roles, roleKey)
	out.IsTruncated = aws.Bool(out.Marker != nil)
	encodeRoleDocuments(out.Roles...)
	return out, nil
}

// DeleteRole implements the IAM DeleteRole action, deleting an IAM role. It returns
// MissingParameter when RoleName is absent, then calls the IAM service for accountID.
func DeleteRole(accountID string, input *iam.DeleteRoleInput, svc handlers_iam.IAMService) (*iam.DeleteRoleOutput, error) {
	if input.RoleName == nil || *input.RoleName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.DeleteRole(accountID, input)
}

// UpdateRole implements the IAM UpdateRole action, updating a role's description or maximum
// session duration. It returns MissingParameter when RoleName is absent, then calls the IAM
// service for accountID.
func UpdateRole(accountID string, input *iam.UpdateRoleInput, svc handlers_iam.IAMService) (*iam.UpdateRoleOutput, error) {
	if input.RoleName == nil || *input.RoleName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.UpdateRole(accountID, input)
}

// UpdateRoleDescription implements the IAM UpdateRoleDescription action. RoleName and Description
// must be present (an empty Description clears it); the trust policy is percent- encoded.
func UpdateRoleDescription(accountID string, input *iam.UpdateRoleDescriptionInput, svc handlers_iam.IAMService) (*iam.UpdateRoleDescriptionOutput, error) {
	if input.RoleName == nil || *input.RoleName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	// Description may be empty, which clears it, but must be present.
	if input.Description == nil {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	out, err := svc.UpdateRoleDescription(accountID, input)
	if err != nil {
		return nil, err
	}
	encodeRoleDocuments(out.Role)
	return out, nil
}

// UpdateAssumeRolePolicy implements the IAM UpdateAssumeRolePolicy action, replacing a role's
// trust policy. It returns MissingParameter when RoleName or PolicyDocument is absent, then calls
// the IAM service for accountID.
func UpdateAssumeRolePolicy(accountID string, input *iam.UpdateAssumeRolePolicyInput, svc handlers_iam.IAMService) (*iam.UpdateAssumeRolePolicyOutput, error) {
	if input.RoleName == nil || *input.RoleName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.PolicyDocument == nil || *input.PolicyDocument == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.UpdateAssumeRolePolicy(accountID, input)
}

// AttachRolePolicy implements the IAM AttachRolePolicy action, attaching a managed policy to a
// role. It returns MissingParameter when RoleName or PolicyArn is absent, then calls the IAM
// service for accountID.
func AttachRolePolicy(accountID string, input *iam.AttachRolePolicyInput, svc handlers_iam.IAMService) (*iam.AttachRolePolicyOutput, error) {
	if input.RoleName == nil || *input.RoleName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.PolicyArn == nil || *input.PolicyArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.AttachRolePolicy(accountID, input)
}

// DetachRolePolicy implements the IAM DetachRolePolicy action, detaching a managed policy from a
// role. It returns MissingParameter when RoleName or PolicyArn is absent, then calls the IAM
// service for accountID.
func DetachRolePolicy(accountID string, input *iam.DetachRolePolicyInput, svc handlers_iam.IAMService) (*iam.DetachRolePolicyOutput, error) {
	if input.RoleName == nil || *input.RoleName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.PolicyArn == nil || *input.PolicyArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.DetachRolePolicy(accountID, input)
}

// ListAttachedRolePolicies implements the IAM ListAttachedRolePolicies action, listing a role's
// managed policies. It requires RoleName and validates PathPrefix; results are paged here by
// Marker and MaxItems.
func ListAttachedRolePolicies(accountID string, input *iam.ListAttachedRolePoliciesInput, svc handlers_iam.IAMService) (*iam.ListAttachedRolePoliciesOutput, error) {
	if input.RoleName == nil || *input.RoleName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if err := validatePathPrefix(input.PathPrefix, policyPathPrefix); err != nil {
		return nil, err
	}
	p, err := newPager(input.Marker, input.MaxItems)
	if err != nil {
		return nil, err
	}
	out, err := svc.ListAttachedRolePolicies(accountID, input)
	if err != nil {
		return nil, err
	}
	out.AttachedPolicies, out.Marker = paginate(p, out.AttachedPolicies, attachedPolicyKey)
	out.IsTruncated = aws.Bool(out.Marker != nil)
	return out, nil
}

// PutRolePolicy implements the IAM PutRolePolicy action, adding or replacing a role inline
// policy. It returns MissingParameter when RoleName, PolicyName or PolicyDocument is absent, then
// calls the IAM service for accountID.
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

// GetRolePolicy implements the IAM GetRolePolicy action, reading a role inline policy. It
// requires RoleName or PolicyName and returns the document percent-encoded as the IAM Query API
// does.
func GetRolePolicy(accountID string, input *iam.GetRolePolicyInput, svc handlers_iam.IAMService) (*iam.GetRolePolicyOutput, error) {
	if input.RoleName == nil || *input.RoleName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.PolicyName == nil || *input.PolicyName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	out, err := svc.GetRolePolicy(accountID, input)
	if err != nil {
		return nil, err
	}
	out.PolicyDocument = encodePolicyDocument(out.PolicyDocument)
	return out, nil
}

// DeleteRolePolicy implements the IAM DeleteRolePolicy action, deleting a role inline policy. It
// returns MissingParameter when RoleName or PolicyName is absent, then calls the IAM service for
// accountID.
func DeleteRolePolicy(accountID string, input *iam.DeleteRolePolicyInput, svc handlers_iam.IAMService) (*iam.DeleteRolePolicyOutput, error) {
	if input.RoleName == nil || *input.RoleName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.PolicyName == nil || *input.PolicyName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.DeleteRolePolicy(accountID, input)
}

// ListRolePolicies implements the IAM ListRolePolicies action, listing a role's inline policy
// names. It requires RoleName; results are paged here by Marker and MaxItems.
func ListRolePolicies(accountID string, input *iam.ListRolePoliciesInput, svc handlers_iam.IAMService) (*iam.ListRolePoliciesOutput, error) {
	if input.RoleName == nil || *input.RoleName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	p, err := newPager(input.Marker, input.MaxItems)
	if err != nil {
		return nil, err
	}
	out, err := svc.ListRolePolicies(accountID, input)
	if err != nil {
		return nil, err
	}
	out.PolicyNames, out.Marker = paginate(p, out.PolicyNames, aws.StringValue)
	out.IsTruncated = aws.Bool(out.Marker != nil)
	return out, nil
}

// TagRole implements the IAM TagRole action. It returns MissingParameter when RoleName is absent
// or Tags is empty.
func TagRole(accountID string, input *iam.TagRoleInput, svc handlers_iam.IAMService) (*iam.TagRoleOutput, error) {
	if input.RoleName == nil || *input.RoleName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if len(input.Tags) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.TagRole(accountID, input)
}

// UntagRole implements the IAM UntagRole action. It returns MissingParameter when RoleName is
// absent or TagKeys is empty.
func UntagRole(accountID string, input *iam.UntagRoleInput, svc handlers_iam.IAMService) (*iam.UntagRoleOutput, error) {
	if input.RoleName == nil || *input.RoleName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if len(input.TagKeys) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.UntagRole(accountID, input)
}

// ListRoleTags implements the IAM ListRoleTags action, listing a role's tags. It requires
// RoleName; results are paged here by Marker and MaxItems.
func ListRoleTags(accountID string, input *iam.ListRoleTagsInput, svc handlers_iam.IAMService) (*iam.ListRoleTagsOutput, error) {
	if input.RoleName == nil || *input.RoleName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	p, err := newPager(input.Marker, input.MaxItems)
	if err != nil {
		return nil, err
	}
	out, err := svc.ListRoleTags(accountID, input)
	if err != nil {
		return nil, err
	}
	out.Tags, out.Marker = paginate(p, out.Tags, tagKey)
	out.IsTruncated = aws.Bool(out.Marker != nil)
	return out, nil
}
