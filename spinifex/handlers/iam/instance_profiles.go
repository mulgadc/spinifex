package handlers_iam

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// InstanceProfile CRUD and role↔profile binding are implemented in Step 6
// of docs/development/feature/iam-roles-v1.md. The stubs below satisfy the
// IAMService interface until that step lands.

func (s *IAMServiceImpl) CreateInstanceProfile(accountID string, input *iam.CreateInstanceProfileInput) (*iam.CreateInstanceProfileOutput, error) {
	return nil, errors.New(awserrors.ErrorInvalidAction)
}

func (s *IAMServiceImpl) GetInstanceProfile(accountID string, input *iam.GetInstanceProfileInput) (*iam.GetInstanceProfileOutput, error) {
	return nil, errors.New(awserrors.ErrorInvalidAction)
}

func (s *IAMServiceImpl) ListInstanceProfiles(accountID string, input *iam.ListInstanceProfilesInput) (*iam.ListInstanceProfilesOutput, error) {
	return nil, errors.New(awserrors.ErrorInvalidAction)
}

func (s *IAMServiceImpl) DeleteInstanceProfile(accountID string, input *iam.DeleteInstanceProfileInput) (*iam.DeleteInstanceProfileOutput, error) {
	return nil, errors.New(awserrors.ErrorInvalidAction)
}

func (s *IAMServiceImpl) ListInstanceProfilesForRole(accountID string, input *iam.ListInstanceProfilesForRoleInput) (*iam.ListInstanceProfilesForRoleOutput, error) {
	return nil, errors.New(awserrors.ErrorInvalidAction)
}

func (s *IAMServiceImpl) AddRoleToInstanceProfile(accountID string, input *iam.AddRoleToInstanceProfileInput) (*iam.AddRoleToInstanceProfileOutput, error) {
	return nil, errors.New(awserrors.ErrorInvalidAction)
}

func (s *IAMServiceImpl) RemoveRoleFromInstanceProfile(accountID string, input *iam.RemoveRoleFromInstanceProfileInput) (*iam.RemoveRoleFromInstanceProfileOutput, error) {
	return nil, errors.New(awserrors.ErrorInvalidAction)
}
