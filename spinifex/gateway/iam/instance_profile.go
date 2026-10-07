package gateway_iam

import (
	"errors"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
)

// LiveAssociationCounter reports how many live EC2 instances reference the
// given profile ARN. DeleteInstanceProfile uses it to refuse delete-while-in-use.
type LiveAssociationCounter func(profileARN string) (int, error)

// CreateInstanceProfile implements the IAM CreateInstanceProfile action, creating an instance
// profile. It returns MissingParameter when InstanceProfileName is absent, then calls the IAM
// service for accountID.
func CreateInstanceProfile(accountID string, input *iam.CreateInstanceProfileInput, svc handlers_iam.IAMService) (*iam.CreateInstanceProfileOutput, error) {
	if input.InstanceProfileName == nil || *input.InstanceProfileName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.CreateInstanceProfile(accountID, input)
}

// GetInstanceProfile implements the IAM GetInstanceProfile action. It requires
// InstanceProfileName and percent-encodes each role's trust policy as the IAM Query API does.
func GetInstanceProfile(accountID string, input *iam.GetInstanceProfileInput, svc handlers_iam.IAMService) (*iam.GetInstanceProfileOutput, error) {
	if input.InstanceProfileName == nil || *input.InstanceProfileName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	out, err := svc.GetInstanceProfile(accountID, input)
	if err != nil {
		return nil, err
	}
	encodeInstanceProfileDocuments(out.InstanceProfile)
	return out, nil
}

// ListInstanceProfiles implements the IAM ListInstanceProfiles action. It validates PathPrefix,
// pages results here by Marker and MaxItems, and percent-encodes role trust policies.
func ListInstanceProfiles(accountID string, input *iam.ListInstanceProfilesInput, svc handlers_iam.IAMService) (*iam.ListInstanceProfilesOutput, error) {
	if err := validatePathPrefix(input.PathPrefix, identityPathPrefix); err != nil {
		return nil, err
	}
	p, err := newPager(input.Marker, input.MaxItems)
	if err != nil {
		return nil, err
	}
	out, err := svc.ListInstanceProfiles(accountID, input)
	if err != nil {
		return nil, err
	}
	out.InstanceProfiles, out.Marker = paginate(p, out.InstanceProfiles, instanceProfileKey)
	out.IsTruncated = aws.Bool(out.Marker != nil)
	encodeInstanceProfileDocuments(out.InstanceProfiles...)
	return out, nil
}

// DeleteInstanceProfile refuses to delete a profile still referenced by a live
// VM. countLive fans out to all daemons; a non-zero count returns DeleteConflict.
// The RoleName-attached guard runs inside svc.DeleteInstanceProfile afterward.
// countLive may be nil (e.g. in unit tests); the live-instance check is skipped.
func DeleteInstanceProfile(accountID string, input *iam.DeleteInstanceProfileInput, svc handlers_iam.IAMService, countLive LiveAssociationCounter) (*iam.DeleteInstanceProfileOutput, error) {
	if input.InstanceProfileName == nil || *input.InstanceProfileName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	if countLive != nil {
		getOut, err := svc.GetInstanceProfile(accountID, &iam.GetInstanceProfileInput{
			InstanceProfileName: input.InstanceProfileName,
		})
		if err != nil {
			return nil, err
		}
		profileARN := aws.StringValue(getOut.InstanceProfile.Arn)
		if profileARN != "" {
			count, err := countLive(profileARN)
			if err != nil {
				return nil, err
			}
			if count > 0 {
				slog.Info("DeleteInstanceProfile refused: profile in use",
					"accountID", accountID,
					"instanceProfileName", *input.InstanceProfileName,
					"profileARN", profileARN,
					"liveAssociations", count)
				return nil, errors.New(awserrors.ErrorIAMDeleteConflict)
			}
		}
	}

	return svc.DeleteInstanceProfile(accountID, input)
}

// ListInstanceProfilesForRole implements the IAM action of the same name. It requires RoleName,
// pages results here by Marker and MaxItems, and percent-encodes role trust policies.
func ListInstanceProfilesForRole(accountID string, input *iam.ListInstanceProfilesForRoleInput, svc handlers_iam.IAMService) (*iam.ListInstanceProfilesForRoleOutput, error) {
	if input.RoleName == nil || *input.RoleName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	p, err := newPager(input.Marker, input.MaxItems)
	if err != nil {
		return nil, err
	}
	out, err := svc.ListInstanceProfilesForRole(accountID, input)
	if err != nil {
		return nil, err
	}
	out.InstanceProfiles, out.Marker = paginate(p, out.InstanceProfiles, instanceProfileKey)
	out.IsTruncated = aws.Bool(out.Marker != nil)
	encodeInstanceProfileDocuments(out.InstanceProfiles...)
	return out, nil
}

// AddRoleToInstanceProfile implements the IAM AddRoleToInstanceProfile action, adding a role to
// an instance profile. It returns MissingParameter when InstanceProfileName or RoleName is
// absent, then calls the IAM service for accountID.
func AddRoleToInstanceProfile(accountID string, input *iam.AddRoleToInstanceProfileInput, svc handlers_iam.IAMService) (*iam.AddRoleToInstanceProfileOutput, error) {
	if input.InstanceProfileName == nil || *input.InstanceProfileName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.RoleName == nil || *input.RoleName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.AddRoleToInstanceProfile(accountID, input)
}

// RemoveRoleFromInstanceProfile implements the IAM RemoveRoleFromInstanceProfile action, removing
// a role from an instance profile. It returns MissingParameter when InstanceProfileName or
// RoleName is absent, then calls the IAM service for accountID.
func RemoveRoleFromInstanceProfile(accountID string, input *iam.RemoveRoleFromInstanceProfileInput, svc handlers_iam.IAMService) (*iam.RemoveRoleFromInstanceProfileOutput, error) {
	if input.InstanceProfileName == nil || *input.InstanceProfileName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.RoleName == nil || *input.RoleName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.RemoveRoleFromInstanceProfile(accountID, input)
}

// TagInstanceProfile implements the IAM TagInstanceProfile action. It returns MissingParameter
// when InstanceProfileName is absent or Tags is empty.
func TagInstanceProfile(accountID string, input *iam.TagInstanceProfileInput, svc handlers_iam.IAMService) (*iam.TagInstanceProfileOutput, error) {
	if input.InstanceProfileName == nil || *input.InstanceProfileName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if len(input.Tags) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.TagInstanceProfile(accountID, input)
}

// UntagInstanceProfile implements the IAM UntagInstanceProfile action. It returns
// MissingParameter when InstanceProfileName is absent or TagKeys is empty.
func UntagInstanceProfile(accountID string, input *iam.UntagInstanceProfileInput, svc handlers_iam.IAMService) (*iam.UntagInstanceProfileOutput, error) {
	if input.InstanceProfileName == nil || *input.InstanceProfileName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if len(input.TagKeys) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.UntagInstanceProfile(accountID, input)
}

// ListInstanceProfileTags implements the IAM ListInstanceProfileTags action, listing an instance
// profile's tags. It requires InstanceProfileName; results are paged here by Marker and MaxItems.
func ListInstanceProfileTags(accountID string, input *iam.ListInstanceProfileTagsInput, svc handlers_iam.IAMService) (*iam.ListInstanceProfileTagsOutput, error) {
	if input.InstanceProfileName == nil || *input.InstanceProfileName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	p, err := newPager(input.Marker, input.MaxItems)
	if err != nil {
		return nil, err
	}
	out, err := svc.ListInstanceProfileTags(accountID, input)
	if err != nil {
		return nil, err
	}
	out.Tags, out.Marker = paginate(p, out.Tags, tagKey)
	out.IsTruncated = aws.Bool(out.Marker != nil)
	return out, nil
}
