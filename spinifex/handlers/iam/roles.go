package handlers_iam

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// Role CRUD, role-policy attachment, and trust-policy validation are
// implemented in Steps 4-5 of docs/development/feature/iam-roles-v1.md.
// The stubs below satisfy the IAMService interface until those steps land.

func (s *IAMServiceImpl) CreateRole(accountID string, input *iam.CreateRoleInput) (*iam.CreateRoleOutput, error) {
	return nil, errors.New(awserrors.ErrorInvalidAction)
}

func (s *IAMServiceImpl) GetRole(accountID string, input *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
	return nil, errors.New(awserrors.ErrorInvalidAction)
}

func (s *IAMServiceImpl) ListRoles(accountID string, input *iam.ListRolesInput) (*iam.ListRolesOutput, error) {
	return nil, errors.New(awserrors.ErrorInvalidAction)
}

func (s *IAMServiceImpl) DeleteRole(accountID string, input *iam.DeleteRoleInput) (*iam.DeleteRoleOutput, error) {
	return nil, errors.New(awserrors.ErrorInvalidAction)
}

func (s *IAMServiceImpl) UpdateRole(accountID string, input *iam.UpdateRoleInput) (*iam.UpdateRoleOutput, error) {
	return nil, errors.New(awserrors.ErrorInvalidAction)
}

func (s *IAMServiceImpl) UpdateAssumeRolePolicy(accountID string, input *iam.UpdateAssumeRolePolicyInput) (*iam.UpdateAssumeRolePolicyOutput, error) {
	return nil, errors.New(awserrors.ErrorInvalidAction)
}

func (s *IAMServiceImpl) AttachRolePolicy(accountID string, input *iam.AttachRolePolicyInput) (*iam.AttachRolePolicyOutput, error) {
	return nil, errors.New(awserrors.ErrorInvalidAction)
}

func (s *IAMServiceImpl) DetachRolePolicy(accountID string, input *iam.DetachRolePolicyInput) (*iam.DetachRolePolicyOutput, error) {
	return nil, errors.New(awserrors.ErrorInvalidAction)
}

func (s *IAMServiceImpl) ListAttachedRolePolicies(accountID string, input *iam.ListAttachedRolePoliciesInput) (*iam.ListAttachedRolePoliciesOutput, error) {
	return nil, errors.New(awserrors.ErrorInvalidAction)
}
