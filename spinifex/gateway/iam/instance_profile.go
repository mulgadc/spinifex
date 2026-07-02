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

func CreateInstanceProfile(accountID string, input *iam.CreateInstanceProfileInput, svc handlers_iam.IAMService) (*iam.CreateInstanceProfileOutput, error) {
	if input.InstanceProfileName == nil || *input.InstanceProfileName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.CreateInstanceProfile(accountID, input)
}

func GetInstanceProfile(accountID string, input *iam.GetInstanceProfileInput, svc handlers_iam.IAMService) (*iam.GetInstanceProfileOutput, error) {
	if input.InstanceProfileName == nil || *input.InstanceProfileName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.GetInstanceProfile(accountID, input)
}

func ListInstanceProfiles(accountID string, input *iam.ListInstanceProfilesInput, svc handlers_iam.IAMService) (*iam.ListInstanceProfilesOutput, error) {
	return svc.ListInstanceProfiles(accountID, input)
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

func ListInstanceProfilesForRole(accountID string, input *iam.ListInstanceProfilesForRoleInput, svc handlers_iam.IAMService) (*iam.ListInstanceProfilesForRoleOutput, error) {
	if input.RoleName == nil || *input.RoleName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.ListInstanceProfilesForRole(accountID, input)
}

func AddRoleToInstanceProfile(accountID string, input *iam.AddRoleToInstanceProfileInput, svc handlers_iam.IAMService) (*iam.AddRoleToInstanceProfileOutput, error) {
	if input.InstanceProfileName == nil || *input.InstanceProfileName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.RoleName == nil || *input.RoleName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.AddRoleToInstanceProfile(accountID, input)
}

func RemoveRoleFromInstanceProfile(accountID string, input *iam.RemoveRoleFromInstanceProfileInput, svc handlers_iam.IAMService) (*iam.RemoveRoleFromInstanceProfileOutput, error) {
	if input.InstanceProfileName == nil || *input.InstanceProfileName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.RoleName == nil || *input.RoleName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.RemoveRoleFromInstanceProfile(accountID, input)
}

func TagInstanceProfile(accountID string, input *iam.TagInstanceProfileInput, svc handlers_iam.IAMService) (*iam.TagInstanceProfileOutput, error) {
	if input.InstanceProfileName == nil || *input.InstanceProfileName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if len(input.Tags) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.TagInstanceProfile(accountID, input)
}

func UntagInstanceProfile(accountID string, input *iam.UntagInstanceProfileInput, svc handlers_iam.IAMService) (*iam.UntagInstanceProfileOutput, error) {
	if input.InstanceProfileName == nil || *input.InstanceProfileName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if len(input.TagKeys) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.UntagInstanceProfile(accountID, input)
}

func ListInstanceProfileTags(accountID string, input *iam.ListInstanceProfileTagsInput, svc handlers_iam.IAMService) (*iam.ListInstanceProfileTagsOutput, error) {
	if input.InstanceProfileName == nil || *input.InstanceProfileName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return svc.ListInstanceProfileTags(accountID, input)
}
