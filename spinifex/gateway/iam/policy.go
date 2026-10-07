package gateway_iam

import (
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
)

// The model's arnType length bounds.
const (
	minARNLength = 20
	maxARNLength = 2048
)

// CreatePolicy implements the IAM CreatePolicy action, creating a customer managed policy. It
// returns MissingParameter when PolicyName or PolicyDocument is absent, then calls the IAM
// service for accountID.
func CreatePolicy(accountID string, input *iam.CreatePolicyInput, svc handlers_iam.IAMService) (*iam.CreatePolicyOutput, error) {
	if input.PolicyName == nil || *input.PolicyName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.PolicyDocument == nil || *input.PolicyDocument == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.CreatePolicy(accountID, input)
}

// GetPolicy implements the IAM GetPolicy action, describing a managed policy. It returns
// MissingParameter when PolicyArn is absent, then calls the IAM service for accountID.
func GetPolicy(accountID string, input *iam.GetPolicyInput, svc handlers_iam.IAMService) (*iam.GetPolicyOutput, error) {
	if input.PolicyArn == nil || *input.PolicyArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.GetPolicy(accountID, input)
}

// GetPolicyVersion implements the IAM GetPolicyVersion action, reading one version of a managed
// policy. It requires PolicyArn or VersionId and returns the document percent-encoded as the IAM
// Query API does.
func GetPolicyVersion(accountID string, input *iam.GetPolicyVersionInput, svc handlers_iam.IAMService) (*iam.GetPolicyVersionOutput, error) {
	if input.PolicyArn == nil || *input.PolicyArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.VersionId == nil || *input.VersionId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	out, err := svc.GetPolicyVersion(accountID, input)
	if err != nil {
		return nil, err
	}
	if out.PolicyVersion != nil {
		out.PolicyVersion.Document = encodePolicyDocument(out.PolicyVersion.Document)
	}
	return out, nil
}

// ListPolicyVersions implements the IAM ListPolicyVersions action. It requires PolicyArn;
// versions are paged here newest first by Marker and MaxItems.
func ListPolicyVersions(accountID string, input *iam.ListPolicyVersionsInput, svc handlers_iam.IAMService) (*iam.ListPolicyVersionsOutput, error) {
	if input.PolicyArn == nil || *input.PolicyArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	p, err := newPager(input.Marker, input.MaxItems)
	if err != nil {
		return nil, err
	}
	out, err := svc.ListPolicyVersions(accountID, input)
	if err != nil {
		return nil, err
	}
	out.Versions, out.Marker = paginateFunc(p, out.Versions, policyVersionKey, newestVersionFirst)
	out.IsTruncated = aws.Bool(out.Marker != nil)
	return out, nil
}

// CreatePolicyVersion implements the IAM CreatePolicyVersion action, adding a new version to a
// managed policy. It returns MissingParameter when PolicyArn or PolicyDocument is absent, then
// calls the IAM service for accountID.
func CreatePolicyVersion(accountID string, input *iam.CreatePolicyVersionInput, svc handlers_iam.IAMService) (*iam.CreatePolicyVersionOutput, error) {
	if input.PolicyArn == nil || *input.PolicyArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.PolicyDocument == nil || *input.PolicyDocument == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.CreatePolicyVersion(accountID, input)
}

// SetDefaultPolicyVersion implements the IAM SetDefaultPolicyVersion action, choosing a managed
// policy's default version. It returns MissingParameter when PolicyArn or VersionId is absent,
// then calls the IAM service for accountID.
func SetDefaultPolicyVersion(accountID string, input *iam.SetDefaultPolicyVersionInput, svc handlers_iam.IAMService) (*iam.SetDefaultPolicyVersionOutput, error) {
	if input.PolicyArn == nil || *input.PolicyArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.VersionId == nil || *input.VersionId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.SetDefaultPolicyVersion(accountID, input)
}

// DeletePolicyVersion implements the IAM DeletePolicyVersion action, deleting a non-default
// policy version. It returns MissingParameter when PolicyArn or VersionId is absent, then calls
// the IAM service for accountID.
func DeletePolicyVersion(accountID string, input *iam.DeletePolicyVersionInput, svc handlers_iam.IAMService) (*iam.DeletePolicyVersionOutput, error) {
	if input.PolicyArn == nil || *input.PolicyArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.VersionId == nil || *input.VersionId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.DeletePolicyVersion(accountID, input)
}

// ListPolicies implements the IAM ListPolicies action, listing managed policies. It validates
// PathPrefix; results are paged here by Marker and MaxItems.
func ListPolicies(accountID string, input *iam.ListPoliciesInput, svc handlers_iam.IAMService) (*iam.ListPoliciesOutput, error) {
	if err := validatePathPrefix(input.PathPrefix, policyPathPrefix); err != nil {
		return nil, err
	}
	p, err := newPager(input.Marker, input.MaxItems)
	if err != nil {
		return nil, err
	}
	out, err := svc.ListPolicies(accountID, input)
	if err != nil {
		return nil, err
	}
	out.Policies, out.Marker = paginate(p, out.Policies, policyKey)
	out.IsTruncated = aws.Bool(out.Marker != nil)
	return out, nil
}

// DeletePolicy implements the IAM DeletePolicy action, deleting a customer managed policy. It
// returns MissingParameter when PolicyArn is absent, then calls the IAM service for accountID.
func DeletePolicy(accountID string, input *iam.DeletePolicyInput, svc handlers_iam.IAMService) (*iam.DeletePolicyOutput, error) {
	if input.PolicyArn == nil || *input.PolicyArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.DeletePolicy(accountID, input)
}

// ListEntitiesForPolicy implements the IAM ListEntitiesForPolicy action. It checks PolicyArn's
// length and PathPrefix as the IAM model does, then pages groups, roles and users as one
// sequence.
func ListEntitiesForPolicy(accountID string, input *iam.ListEntitiesForPolicyInput, svc handlers_iam.IAMService) (*iam.ListEntitiesForPolicyOutput, error) {
	if input.PolicyArn == nil || *input.PolicyArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if n := len(*input.PolicyArn); n < minARNLength || n > maxARNLength {
		bound := fmt.Sprintf("greater than or equal to %d", minARNLength)
		if n > maxARNLength {
			bound = fmt.Sprintf("less than or equal to %d", maxARNLength)
		}
		return nil, awserrors.Errorf(awserrors.ErrorValidationError,
			"1 validation error detected: Value at 'policyArn' failed to satisfy constraint: Member must have length %s", bound)
	}
	if err := validatePathPrefix(input.PathPrefix, entityPathPrefix); err != nil {
		return nil, err
	}
	p, err := newPager(input.Marker, input.MaxItems)
	if err != nil {
		return nil, err
	}
	out, err := svc.ListEntitiesForPolicy(accountID, input)
	if err != nil {
		return nil, err
	}
	pageEntitiesForPolicy(p, out)
	return out, nil
}

// policyEntity is one row of ListEntitiesForPolicy's three lists, which page as
// a single sequence: groups, then roles, then users, each by name.
type policyEntity struct {
	key   string
	group *iam.PolicyGroup
	role  *iam.PolicyRole
	user  *iam.PolicyUser
}

func pageEntitiesForPolicy(p pager, out *iam.ListEntitiesForPolicyOutput) {
	all := make([]policyEntity, 0, len(out.PolicyGroups)+len(out.PolicyRoles)+len(out.PolicyUsers))
	for _, g := range out.PolicyGroups {
		all = append(all, policyEntity{key: iam.EntityTypeGroup + "\x00" + aws.StringValue(g.GroupName), group: g})
	}
	for _, r := range out.PolicyRoles {
		all = append(all, policyEntity{key: iam.EntityTypeRole + "\x00" + aws.StringValue(r.RoleName), role: r})
	}
	for _, u := range out.PolicyUsers {
		all = append(all, policyEntity{key: iam.EntityTypeUser + "\x00" + aws.StringValue(u.UserName), user: u})
	}

	page, marker := paginate(p, all, func(e policyEntity) string { return e.key })
	out.PolicyGroups, out.PolicyRoles, out.PolicyUsers = []*iam.PolicyGroup{}, []*iam.PolicyRole{}, []*iam.PolicyUser{}
	for _, e := range page {
		switch {
		case e.group != nil:
			out.PolicyGroups = append(out.PolicyGroups, e.group)
		case e.role != nil:
			out.PolicyRoles = append(out.PolicyRoles, e.role)
		default:
			out.PolicyUsers = append(out.PolicyUsers, e.user)
		}
	}
	out.Marker, out.IsTruncated = marker, aws.Bool(marker != nil)
}

// AttachUserPolicy implements the IAM AttachUserPolicy action, attaching a managed policy to a
// user. It returns MissingParameter when UserName or PolicyArn is absent, then calls the IAM
// service for accountID.
func AttachUserPolicy(accountID string, input *iam.AttachUserPolicyInput, svc handlers_iam.IAMService) (*iam.AttachUserPolicyOutput, error) {
	if input.UserName == nil || *input.UserName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.PolicyArn == nil || *input.PolicyArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.AttachUserPolicy(accountID, input)
}

// DetachUserPolicy implements the IAM DetachUserPolicy action, detaching a managed policy from a
// user. It returns MissingParameter when UserName or PolicyArn is absent, then calls the IAM
// service for accountID.
func DetachUserPolicy(accountID string, input *iam.DetachUserPolicyInput, svc handlers_iam.IAMService) (*iam.DetachUserPolicyOutput, error) {
	if input.UserName == nil || *input.UserName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.PolicyArn == nil || *input.PolicyArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.DetachUserPolicy(accountID, input)
}

// ListAttachedUserPolicies implements the IAM ListAttachedUserPolicies action, listing a user's
// managed policies. It requires UserName and validates PathPrefix; results are paged here by
// Marker and MaxItems.
func ListAttachedUserPolicies(accountID string, input *iam.ListAttachedUserPoliciesInput, svc handlers_iam.IAMService) (*iam.ListAttachedUserPoliciesOutput, error) {
	if input.UserName == nil || *input.UserName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if err := validatePathPrefix(input.PathPrefix, policyPathPrefix); err != nil {
		return nil, err
	}
	p, err := newPager(input.Marker, input.MaxItems)
	if err != nil {
		return nil, err
	}
	out, err := svc.ListAttachedUserPolicies(accountID, input)
	if err != nil {
		return nil, err
	}
	out.AttachedPolicies, out.Marker = paginate(p, out.AttachedPolicies, attachedPolicyKey)
	out.IsTruncated = aws.Bool(out.Marker != nil)
	return out, nil
}

// TagPolicy implements the IAM TagPolicy action. It returns MissingParameter when PolicyArn is
// absent or Tags is empty.
func TagPolicy(accountID string, input *iam.TagPolicyInput, svc handlers_iam.IAMService) (*iam.TagPolicyOutput, error) {
	if input.PolicyArn == nil || *input.PolicyArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if len(input.Tags) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.TagPolicy(accountID, input)
}

// UntagPolicy implements the IAM UntagPolicy action. It returns MissingParameter when PolicyArn
// is absent or TagKeys is empty.
func UntagPolicy(accountID string, input *iam.UntagPolicyInput, svc handlers_iam.IAMService) (*iam.UntagPolicyOutput, error) {
	if input.PolicyArn == nil || *input.PolicyArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if len(input.TagKeys) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.UntagPolicy(accountID, input)
}

// ListPolicyTags implements the IAM ListPolicyTags action, listing a managed policy's tags. It
// requires PolicyArn; results are paged here by Marker and MaxItems.
func ListPolicyTags(accountID string, input *iam.ListPolicyTagsInput, svc handlers_iam.IAMService) (*iam.ListPolicyTagsOutput, error) {
	if input.PolicyArn == nil || *input.PolicyArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	p, err := newPager(input.Marker, input.MaxItems)
	if err != nil {
		return nil, err
	}
	out, err := svc.ListPolicyTags(accountID, input)
	if err != nil {
		return nil, err
	}
	out.Tags, out.Marker = paginate(p, out.Tags, tagKey)
	out.IsTruncated = aws.Bool(out.Marker != nil)
	return out, nil
}
