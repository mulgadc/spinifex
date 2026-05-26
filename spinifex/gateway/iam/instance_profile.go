package gateway_iam

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
)

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

func DeleteInstanceProfile(accountID string, input *iam.DeleteInstanceProfileInput, svc handlers_iam.IAMService) (*iam.DeleteInstanceProfileOutput, error) {
	if input.InstanceProfileName == nil || *input.InstanceProfileName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
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
