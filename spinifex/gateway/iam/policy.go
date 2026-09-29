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
	out, err := svc.GetPolicyVersion(accountID, input)
	if err != nil {
		return nil, err
	}
	if out.PolicyVersion != nil {
		out.PolicyVersion.Document = encodePolicyDocument(out.PolicyVersion.Document)
	}
	return out, nil
}

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

func CreatePolicyVersion(accountID string, input *iam.CreatePolicyVersionInput, svc handlers_iam.IAMService) (*iam.CreatePolicyVersionOutput, error) {
	if input.PolicyArn == nil || *input.PolicyArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.PolicyDocument == nil || *input.PolicyDocument == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.CreatePolicyVersion(accountID, input)
}

func SetDefaultPolicyVersion(accountID string, input *iam.SetDefaultPolicyVersionInput, svc handlers_iam.IAMService) (*iam.SetDefaultPolicyVersionOutput, error) {
	if input.PolicyArn == nil || *input.PolicyArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.VersionId == nil || *input.VersionId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.SetDefaultPolicyVersion(accountID, input)
}

func DeletePolicyVersion(accountID string, input *iam.DeletePolicyVersionInput, svc handlers_iam.IAMService) (*iam.DeletePolicyVersionOutput, error) {
	if input.PolicyArn == nil || *input.PolicyArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.VersionId == nil || *input.VersionId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.DeletePolicyVersion(accountID, input)
}

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

func DeletePolicy(accountID string, input *iam.DeletePolicyInput, svc handlers_iam.IAMService) (*iam.DeletePolicyOutput, error) {
	if input.PolicyArn == nil || *input.PolicyArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.DeletePolicy(accountID, input)
}

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

func TagPolicy(accountID string, input *iam.TagPolicyInput, svc handlers_iam.IAMService) (*iam.TagPolicyOutput, error) {
	if input.PolicyArn == nil || *input.PolicyArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if len(input.Tags) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.TagPolicy(accountID, input)
}

func UntagPolicy(accountID string, input *iam.UntagPolicyInput, svc handlers_iam.IAMService) (*iam.UntagPolicyOutput, error) {
	if input.PolicyArn == nil || *input.PolicyArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if len(input.TagKeys) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.UntagPolicy(accountID, input)
}

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
