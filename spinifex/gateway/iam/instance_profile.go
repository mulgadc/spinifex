package gateway_iam

import (
	"errors"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
)

// LiveAssociationCounter reports how many live EC2 instance-profile
// associations currently reference the given profile ARN. Implementations
// broadcast on ec2.IamProfileAssociation.describe and filter by ARN.
// DeleteInstanceProfile uses it to refuse delete-while-in-use.
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

// DeleteInstanceProfile refuses to delete a profile that any live VM still
// references. The gateway resolves the profile ARN via svc.GetInstanceProfile,
// then countLive fans out to every daemon to count active associations; a
// non-zero count maps to DeleteConflict. The existing RoleName-attached guard
// runs inside svc.DeleteInstanceProfile after both checks pass.
//
// Race window between the live-instance scan and the bucket delete is
// accepted (Resolved Design Decision #2 in iam-roles-v1-ec2.md): the same
// pattern as DeletePolicy. countLive may be nil — primarily in unit tests that
// don't exercise the fan-out path; in production the gateway always supplies it.
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
