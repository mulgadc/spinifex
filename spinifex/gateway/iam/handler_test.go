package gateway_iam

import (
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

const testAccountID = "000000000000"

// stubIAMService returns empty non-nil outputs for all methods. Individual
// tests can override a method by setting the matching function field.
type stubIAMService struct {
	getInstanceProfile func(string, *iam.GetInstanceProfileInput) (*iam.GetInstanceProfileOutput, error)
}

func (s *stubIAMService) CreateUser(_ string, _ *iam.CreateUserInput) (*iam.CreateUserOutput, error) {
	return &iam.CreateUserOutput{}, nil
}

func (s *stubIAMService) GetUser(_ string, _ *iam.GetUserInput) (*iam.GetUserOutput, error) {
	return &iam.GetUserOutput{}, nil
}

func (s *stubIAMService) ListUsers(_ string, _ *iam.ListUsersInput) (*iam.ListUsersOutput, error) {
	return &iam.ListUsersOutput{}, nil
}

func (s *stubIAMService) DeleteUser(_ string, _ *iam.DeleteUserInput) (*iam.DeleteUserOutput, error) {
	return &iam.DeleteUserOutput{}, nil
}

func (s *stubIAMService) CreateAccessKey(_ string, _ *iam.CreateAccessKeyInput) (*iam.CreateAccessKeyOutput, error) {
	return &iam.CreateAccessKeyOutput{}, nil
}

func (s *stubIAMService) ListAccessKeys(_ string, _ *iam.ListAccessKeysInput) (*iam.ListAccessKeysOutput, error) {
	return &iam.ListAccessKeysOutput{}, nil
}

func (s *stubIAMService) DeleteAccessKey(_ string, _ *iam.DeleteAccessKeyInput) (*iam.DeleteAccessKeyOutput, error) {
	return &iam.DeleteAccessKeyOutput{}, nil
}

func (s *stubIAMService) UpdateAccessKey(_ string, _ *iam.UpdateAccessKeyInput) (*iam.UpdateAccessKeyOutput, error) {
	return &iam.UpdateAccessKeyOutput{}, nil
}

func (s *stubIAMService) CreatePolicy(_ string, _ *iam.CreatePolicyInput) (*iam.CreatePolicyOutput, error) {
	return &iam.CreatePolicyOutput{}, nil
}

func (s *stubIAMService) GetPolicy(_ string, _ *iam.GetPolicyInput) (*iam.GetPolicyOutput, error) {
	return &iam.GetPolicyOutput{}, nil
}

func (s *stubIAMService) GetPolicyVersion(_ string, _ *iam.GetPolicyVersionInput) (*iam.GetPolicyVersionOutput, error) {
	return &iam.GetPolicyVersionOutput{}, nil
}

func (s *stubIAMService) ListPolicyVersions(_ string, _ *iam.ListPolicyVersionsInput) (*iam.ListPolicyVersionsOutput, error) {
	return &iam.ListPolicyVersionsOutput{}, nil
}

func (s *stubIAMService) ListPolicies(_ string, _ *iam.ListPoliciesInput) (*iam.ListPoliciesOutput, error) {
	return &iam.ListPoliciesOutput{}, nil
}

func (s *stubIAMService) DeletePolicy(_ string, _ *iam.DeletePolicyInput) (*iam.DeletePolicyOutput, error) {
	return &iam.DeletePolicyOutput{}, nil
}

func (s *stubIAMService) AttachUserPolicy(_ string, _ *iam.AttachUserPolicyInput) (*iam.AttachUserPolicyOutput, error) {
	return &iam.AttachUserPolicyOutput{}, nil
}

func (s *stubIAMService) DetachUserPolicy(_ string, _ *iam.DetachUserPolicyInput) (*iam.DetachUserPolicyOutput, error) {
	return &iam.DetachUserPolicyOutput{}, nil
}

func (s *stubIAMService) ListAttachedUserPolicies(_ string, _ *iam.ListAttachedUserPoliciesInput) (*iam.ListAttachedUserPoliciesOutput, error) {
	return &iam.ListAttachedUserPoliciesOutput{}, nil
}

func (s *stubIAMService) GetUserPolicies(_, _ string) ([]handlers_iam.PolicyDocument, error) {
	return nil, nil
}

func (s *stubIAMService) GetRolePolicies(_, _ string) ([]handlers_iam.PolicyDocument, error) {
	return nil, nil
}

func (s *stubIAMService) LookupAccessKey(_ string) (*handlers_iam.AccessKey, error) {
	return nil, nil
}

func (s *stubIAMService) DecryptSecret(_ string) (string, error)            { return "", nil }
func (s *stubIAMService) SeedBootstrap(_ *handlers_iam.BootstrapData) error { return nil }
func (s *stubIAMService) IsEmpty() (bool, error)                            { return true, nil }

func (s *stubIAMService) CreateAccount(_ string) (*handlers_iam.Account, error) {
	return nil, nil
}
func (s *stubIAMService) GetAccount(_ string) (*handlers_iam.Account, error) { return nil, nil }
func (s *stubIAMService) ListAccounts() ([]*handlers_iam.Account, error)     { return nil, nil }

func (s *stubIAMService) CreateRole(_ string, _ *iam.CreateRoleInput) (*iam.CreateRoleOutput, error) {
	return &iam.CreateRoleOutput{}, nil
}
func (s *stubIAMService) GetRole(_ string, _ *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
	return &iam.GetRoleOutput{}, nil
}
func (s *stubIAMService) ListRoles(_ string, _ *iam.ListRolesInput) (*iam.ListRolesOutput, error) {
	return &iam.ListRolesOutput{}, nil
}
func (s *stubIAMService) DeleteRole(_ string, _ *iam.DeleteRoleInput) (*iam.DeleteRoleOutput, error) {
	return &iam.DeleteRoleOutput{}, nil
}
func (s *stubIAMService) UpdateRole(_ string, _ *iam.UpdateRoleInput) (*iam.UpdateRoleOutput, error) {
	return &iam.UpdateRoleOutput{}, nil
}
func (s *stubIAMService) UpdateAssumeRolePolicy(_ string, _ *iam.UpdateAssumeRolePolicyInput) (*iam.UpdateAssumeRolePolicyOutput, error) {
	return &iam.UpdateAssumeRolePolicyOutput{}, nil
}
func (s *stubIAMService) AttachRolePolicy(_ string, _ *iam.AttachRolePolicyInput) (*iam.AttachRolePolicyOutput, error) {
	return &iam.AttachRolePolicyOutput{}, nil
}
func (s *stubIAMService) DetachRolePolicy(_ string, _ *iam.DetachRolePolicyInput) (*iam.DetachRolePolicyOutput, error) {
	return &iam.DetachRolePolicyOutput{}, nil
}
func (s *stubIAMService) ListAttachedRolePolicies(_ string, _ *iam.ListAttachedRolePoliciesInput) (*iam.ListAttachedRolePoliciesOutput, error) {
	return &iam.ListAttachedRolePoliciesOutput{}, nil
}
func (s *stubIAMService) PutRolePolicy(_ string, _ *iam.PutRolePolicyInput) (*iam.PutRolePolicyOutput, error) {
	return &iam.PutRolePolicyOutput{}, nil
}
func (s *stubIAMService) GetRolePolicy(_ string, _ *iam.GetRolePolicyInput) (*iam.GetRolePolicyOutput, error) {
	return &iam.GetRolePolicyOutput{}, nil
}
func (s *stubIAMService) DeleteRolePolicy(_ string, _ *iam.DeleteRolePolicyInput) (*iam.DeleteRolePolicyOutput, error) {
	return &iam.DeleteRolePolicyOutput{}, nil
}
func (s *stubIAMService) ListRolePolicies(_ string, _ *iam.ListRolePoliciesInput) (*iam.ListRolePoliciesOutput, error) {
	return &iam.ListRolePoliciesOutput{}, nil
}
func (s *stubIAMService) CreateInstanceProfile(_ string, _ *iam.CreateInstanceProfileInput) (*iam.CreateInstanceProfileOutput, error) {
	return &iam.CreateInstanceProfileOutput{}, nil
}
func (s *stubIAMService) GetInstanceProfile(accountID string, in *iam.GetInstanceProfileInput) (*iam.GetInstanceProfileOutput, error) {
	if s.getInstanceProfile != nil {
		return s.getInstanceProfile(accountID, in)
	}
	return &iam.GetInstanceProfileOutput{InstanceProfile: &iam.InstanceProfile{}}, nil
}
func (s *stubIAMService) ListInstanceProfiles(_ string, _ *iam.ListInstanceProfilesInput) (*iam.ListInstanceProfilesOutput, error) {
	return &iam.ListInstanceProfilesOutput{}, nil
}
func (s *stubIAMService) DeleteInstanceProfile(_ string, _ *iam.DeleteInstanceProfileInput) (*iam.DeleteInstanceProfileOutput, error) {
	return &iam.DeleteInstanceProfileOutput{}, nil
}
func (s *stubIAMService) ListInstanceProfilesForRole(_ string, _ *iam.ListInstanceProfilesForRoleInput) (*iam.ListInstanceProfilesForRoleOutput, error) {
	return &iam.ListInstanceProfilesForRoleOutput{}, nil
}
func (s *stubIAMService) AddRoleToInstanceProfile(_ string, _ *iam.AddRoleToInstanceProfileInput) (*iam.AddRoleToInstanceProfileOutput, error) {
	return &iam.AddRoleToInstanceProfileOutput{}, nil
}
func (s *stubIAMService) RemoveRoleFromInstanceProfile(_ string, _ *iam.RemoveRoleFromInstanceProfileInput) (*iam.RemoveRoleFromInstanceProfileOutput, error) {
	return &iam.RemoveRoleFromInstanceProfileOutput{}, nil
}
func (s *stubIAMService) ResolveInstanceProfile(_, _ string) (*handlers_iam.InstanceProfile, error) {
	return nil, nil
}

func (s *stubIAMService) TagUser(_ string, _ *iam.TagUserInput) (*iam.TagUserOutput, error) {
	return &iam.TagUserOutput{}, nil
}
func (s *stubIAMService) UntagUser(_ string, _ *iam.UntagUserInput) (*iam.UntagUserOutput, error) {
	return &iam.UntagUserOutput{}, nil
}
func (s *stubIAMService) ListUserTags(_ string, _ *iam.ListUserTagsInput) (*iam.ListUserTagsOutput, error) {
	return &iam.ListUserTagsOutput{}, nil
}
func (s *stubIAMService) TagRole(_ string, _ *iam.TagRoleInput) (*iam.TagRoleOutput, error) {
	return &iam.TagRoleOutput{}, nil
}
func (s *stubIAMService) UntagRole(_ string, _ *iam.UntagRoleInput) (*iam.UntagRoleOutput, error) {
	return &iam.UntagRoleOutput{}, nil
}
func (s *stubIAMService) ListRoleTags(_ string, _ *iam.ListRoleTagsInput) (*iam.ListRoleTagsOutput, error) {
	return &iam.ListRoleTagsOutput{}, nil
}
func (s *stubIAMService) TagPolicy(_ string, _ *iam.TagPolicyInput) (*iam.TagPolicyOutput, error) {
	return &iam.TagPolicyOutput{}, nil
}
func (s *stubIAMService) UntagPolicy(_ string, _ *iam.UntagPolicyInput) (*iam.UntagPolicyOutput, error) {
	return &iam.UntagPolicyOutput{}, nil
}
func (s *stubIAMService) ListPolicyTags(_ string, _ *iam.ListPolicyTagsInput) (*iam.ListPolicyTagsOutput, error) {
	return &iam.ListPolicyTagsOutput{}, nil
}
func (s *stubIAMService) TagInstanceProfile(_ string, _ *iam.TagInstanceProfileInput) (*iam.TagInstanceProfileOutput, error) {
	return &iam.TagInstanceProfileOutput{}, nil
}
func (s *stubIAMService) UntagInstanceProfile(_ string, _ *iam.UntagInstanceProfileInput) (*iam.UntagInstanceProfileOutput, error) {
	return &iam.UntagInstanceProfileOutput{}, nil
}
func (s *stubIAMService) ListInstanceProfileTags(_ string, _ *iam.ListInstanceProfileTagsInput) (*iam.ListInstanceProfileTagsOutput, error) {
	return &iam.ListInstanceProfileTagsOutput{}, nil
}
func (s *stubIAMService) TagOpenIDConnectProvider(_ string, _ *iam.TagOpenIDConnectProviderInput) (*iam.TagOpenIDConnectProviderOutput, error) {
	return &iam.TagOpenIDConnectProviderOutput{}, nil
}
func (s *stubIAMService) UntagOpenIDConnectProvider(_ string, _ *iam.UntagOpenIDConnectProviderInput) (*iam.UntagOpenIDConnectProviderOutput, error) {
	return &iam.UntagOpenIDConnectProviderOutput{}, nil
}
func (s *stubIAMService) ListOpenIDConnectProviderTags(_ string, _ *iam.ListOpenIDConnectProviderTagsInput) (*iam.ListOpenIDConnectProviderTagsOutput, error) {
	return &iam.ListOpenIDConnectProviderTagsOutput{}, nil
}

func (s *stubIAMService) CreateGroup(_ string, _ *iam.CreateGroupInput) (*iam.CreateGroupOutput, error) {
	return &iam.CreateGroupOutput{}, nil
}
func (s *stubIAMService) GetGroup(_ string, _ *iam.GetGroupInput) (*iam.GetGroupOutput, error) {
	return &iam.GetGroupOutput{}, nil
}
func (s *stubIAMService) ListGroups(_ string, _ *iam.ListGroupsInput) (*iam.ListGroupsOutput, error) {
	return &iam.ListGroupsOutput{}, nil
}
func (s *stubIAMService) DeleteGroup(_ string, _ *iam.DeleteGroupInput) (*iam.DeleteGroupOutput, error) {
	return &iam.DeleteGroupOutput{}, nil
}
func (s *stubIAMService) AddUserToGroup(_ string, _ *iam.AddUserToGroupInput) (*iam.AddUserToGroupOutput, error) {
	return &iam.AddUserToGroupOutput{}, nil
}
func (s *stubIAMService) RemoveUserFromGroup(_ string, _ *iam.RemoveUserFromGroupInput) (*iam.RemoveUserFromGroupOutput, error) {
	return &iam.RemoveUserFromGroupOutput{}, nil
}
func (s *stubIAMService) ListGroupsForUser(_ string, _ *iam.ListGroupsForUserInput) (*iam.ListGroupsForUserOutput, error) {
	return &iam.ListGroupsForUserOutput{}, nil
}
func (s *stubIAMService) AttachGroupPolicy(_ string, _ *iam.AttachGroupPolicyInput) (*iam.AttachGroupPolicyOutput, error) {
	return &iam.AttachGroupPolicyOutput{}, nil
}
func (s *stubIAMService) DetachGroupPolicy(_ string, _ *iam.DetachGroupPolicyInput) (*iam.DetachGroupPolicyOutput, error) {
	return &iam.DetachGroupPolicyOutput{}, nil
}
func (s *stubIAMService) ListAttachedGroupPolicies(_ string, _ *iam.ListAttachedGroupPoliciesInput) (*iam.ListAttachedGroupPoliciesOutput, error) {
	return &iam.ListAttachedGroupPoliciesOutput{}, nil
}
func (s *stubIAMService) PutGroupPolicy(_ string, _ *iam.PutGroupPolicyInput) (*iam.PutGroupPolicyOutput, error) {
	return &iam.PutGroupPolicyOutput{}, nil
}
func (s *stubIAMService) GetGroupPolicy(_ string, _ *iam.GetGroupPolicyInput) (*iam.GetGroupPolicyOutput, error) {
	return &iam.GetGroupPolicyOutput{}, nil
}
func (s *stubIAMService) DeleteGroupPolicy(_ string, _ *iam.DeleteGroupPolicyInput) (*iam.DeleteGroupPolicyOutput, error) {
	return &iam.DeleteGroupPolicyOutput{}, nil
}
func (s *stubIAMService) ListGroupPolicies(_ string, _ *iam.ListGroupPoliciesInput) (*iam.ListGroupPoliciesOutput, error) {
	return &iam.ListGroupPoliciesOutput{}, nil
}
func (s *stubIAMService) PutUserPolicy(_ string, _ *iam.PutUserPolicyInput) (*iam.PutUserPolicyOutput, error) {
	return &iam.PutUserPolicyOutput{}, nil
}
func (s *stubIAMService) GetUserPolicy(_ string, _ *iam.GetUserPolicyInput) (*iam.GetUserPolicyOutput, error) {
	return &iam.GetUserPolicyOutput{}, nil
}
func (s *stubIAMService) DeleteUserPolicy(_ string, _ *iam.DeleteUserPolicyInput) (*iam.DeleteUserPolicyOutput, error) {
	return &iam.DeleteUserPolicyOutput{}, nil
}
func (s *stubIAMService) ListUserPolicies(_ string, _ *iam.ListUserPoliciesInput) (*iam.ListUserPoliciesOutput, error) {
	return &iam.ListUserPoliciesOutput{}, nil
}

func TestCreateUser(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.CreateUserInput
		wantErr string
	}{
		{"nil UserName", &iam.CreateUserInput{}, awserrors.ErrorMissingParameter},
		{"empty UserName", &iam.CreateUserInput{UserName: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.CreateUserInput{UserName: aws.String("alice")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CreateUser(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestGetUser(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.GetUserInput
		wantErr string
	}{
		{"nil UserName", &iam.GetUserInput{}, awserrors.ErrorMissingParameter},
		{"empty UserName", &iam.GetUserInput{UserName: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.GetUserInput{UserName: aws.String("alice")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := GetUser(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestListUsers(t *testing.T) {
	svc := &stubIAMService{}
	_, err := ListUsers(testAccountID, &iam.ListUsersInput{}, svc)
	require.NoError(t, err)
}

func TestDeleteUser(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.DeleteUserInput
		wantErr string
	}{
		{"nil UserName", &iam.DeleteUserInput{}, awserrors.ErrorMissingParameter},
		{"empty UserName", &iam.DeleteUserInput{UserName: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.DeleteUserInput{UserName: aws.String("alice")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DeleteUser(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestCreateAccessKey(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.CreateAccessKeyInput
		wantErr string
	}{
		{"nil UserName", &iam.CreateAccessKeyInput{}, awserrors.ErrorMissingParameter},
		{"empty UserName", &iam.CreateAccessKeyInput{UserName: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.CreateAccessKeyInput{UserName: aws.String("alice")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CreateAccessKey(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestListAccessKeys(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.ListAccessKeysInput
		wantErr string
	}{
		{"nil UserName", &iam.ListAccessKeysInput{}, awserrors.ErrorMissingParameter},
		{"empty UserName", &iam.ListAccessKeysInput{UserName: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.ListAccessKeysInput{UserName: aws.String("alice")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ListAccessKeys(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestDeleteAccessKey(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.DeleteAccessKeyInput
		wantErr string
	}{
		{"nil UserName", &iam.DeleteAccessKeyInput{AccessKeyId: aws.String("AKIA123")}, awserrors.ErrorMissingParameter},
		{"empty UserName", &iam.DeleteAccessKeyInput{UserName: aws.String(""), AccessKeyId: aws.String("AKIA123")}, awserrors.ErrorMissingParameter},
		{"nil AccessKeyId", &iam.DeleteAccessKeyInput{UserName: aws.String("alice")}, awserrors.ErrorMissingParameter},
		{"empty AccessKeyId", &iam.DeleteAccessKeyInput{UserName: aws.String("alice"), AccessKeyId: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.DeleteAccessKeyInput{UserName: aws.String("alice"), AccessKeyId: aws.String("AKIA123")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DeleteAccessKey(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestUpdateAccessKey(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.UpdateAccessKeyInput
		wantErr string
	}{
		{"nil AccessKeyId", &iam.UpdateAccessKeyInput{Status: aws.String("Active")}, awserrors.ErrorMissingParameter},
		{"empty AccessKeyId", &iam.UpdateAccessKeyInput{AccessKeyId: aws.String(""), Status: aws.String("Active")}, awserrors.ErrorMissingParameter},
		{"nil Status", &iam.UpdateAccessKeyInput{AccessKeyId: aws.String("AKIA123")}, awserrors.ErrorMissingParameter},
		{"empty Status", &iam.UpdateAccessKeyInput{AccessKeyId: aws.String("AKIA123"), Status: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.UpdateAccessKeyInput{AccessKeyId: aws.String("AKIA123"), Status: aws.String("Active")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := UpdateAccessKey(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// --- Policy CRUD ---

func TestCreatePolicy(t *testing.T) {
	svc := &stubIAMService{}
	doc := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`
	tests := []struct {
		name    string
		input   *iam.CreatePolicyInput
		wantErr string
	}{
		{"nil PolicyName", &iam.CreatePolicyInput{PolicyDocument: aws.String(doc)}, awserrors.ErrorMissingParameter},
		{"empty PolicyName", &iam.CreatePolicyInput{PolicyName: aws.String(""), PolicyDocument: aws.String(doc)}, awserrors.ErrorMissingParameter},
		{"nil PolicyDocument", &iam.CreatePolicyInput{PolicyName: aws.String("mypolicy")}, awserrors.ErrorMissingParameter},
		{"empty PolicyDocument", &iam.CreatePolicyInput{PolicyName: aws.String("mypolicy"), PolicyDocument: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.CreatePolicyInput{PolicyName: aws.String("mypolicy"), PolicyDocument: aws.String(doc)}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CreatePolicy(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestGetPolicy(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.GetPolicyInput
		wantErr string
	}{
		{"nil PolicyArn", &iam.GetPolicyInput{}, awserrors.ErrorMissingParameter},
		{"empty PolicyArn", &iam.GetPolicyInput{PolicyArn: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.GetPolicyInput{PolicyArn: aws.String("arn:aws:iam::000000000000:policy/mypolicy")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := GetPolicy(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestGetPolicyVersion(t *testing.T) {
	svc := &stubIAMService{}
	arn := "arn:aws:iam::000000000000:policy/mypolicy"
	tests := []struct {
		name    string
		input   *iam.GetPolicyVersionInput
		wantErr string
	}{
		{"nil PolicyArn", &iam.GetPolicyVersionInput{VersionId: aws.String("v1")}, awserrors.ErrorMissingParameter},
		{"empty PolicyArn", &iam.GetPolicyVersionInput{PolicyArn: aws.String(""), VersionId: aws.String("v1")}, awserrors.ErrorMissingParameter},
		{"nil VersionId", &iam.GetPolicyVersionInput{PolicyArn: aws.String(arn)}, awserrors.ErrorMissingParameter},
		{"empty VersionId", &iam.GetPolicyVersionInput{PolicyArn: aws.String(arn), VersionId: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.GetPolicyVersionInput{PolicyArn: aws.String(arn), VersionId: aws.String("v1")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := GetPolicyVersion(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestListPolicyVersions(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.ListPolicyVersionsInput
		wantErr string
	}{
		{"nil PolicyArn", &iam.ListPolicyVersionsInput{}, awserrors.ErrorMissingParameter},
		{"empty PolicyArn", &iam.ListPolicyVersionsInput{PolicyArn: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.ListPolicyVersionsInput{PolicyArn: aws.String("arn:aws:iam::000000000000:policy/mypolicy")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ListPolicyVersions(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestListPolicies(t *testing.T) {
	svc := &stubIAMService{}
	_, err := ListPolicies(testAccountID, &iam.ListPoliciesInput{}, svc)
	require.NoError(t, err)
}

func TestDeletePolicy(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.DeletePolicyInput
		wantErr string
	}{
		{"nil PolicyArn", &iam.DeletePolicyInput{}, awserrors.ErrorMissingParameter},
		{"empty PolicyArn", &iam.DeletePolicyInput{PolicyArn: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.DeletePolicyInput{PolicyArn: aws.String("arn:aws:iam::000000000000:policy/mypolicy")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DeletePolicy(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// --- Policy Attachment ---

func TestAttachUserPolicy(t *testing.T) {
	svc := &stubIAMService{}
	arn := "arn:aws:iam::000000000000:policy/mypolicy"
	tests := []struct {
		name    string
		input   *iam.AttachUserPolicyInput
		wantErr string
	}{
		{"nil UserName", &iam.AttachUserPolicyInput{PolicyArn: aws.String(arn)}, awserrors.ErrorMissingParameter},
		{"empty UserName", &iam.AttachUserPolicyInput{UserName: aws.String(""), PolicyArn: aws.String(arn)}, awserrors.ErrorMissingParameter},
		{"nil PolicyArn", &iam.AttachUserPolicyInput{UserName: aws.String("alice")}, awserrors.ErrorMissingParameter},
		{"empty PolicyArn", &iam.AttachUserPolicyInput{UserName: aws.String("alice"), PolicyArn: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.AttachUserPolicyInput{UserName: aws.String("alice"), PolicyArn: aws.String(arn)}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := AttachUserPolicy(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestDetachUserPolicy(t *testing.T) {
	svc := &stubIAMService{}
	arn := "arn:aws:iam::000000000000:policy/mypolicy"
	tests := []struct {
		name    string
		input   *iam.DetachUserPolicyInput
		wantErr string
	}{
		{"nil UserName", &iam.DetachUserPolicyInput{PolicyArn: aws.String(arn)}, awserrors.ErrorMissingParameter},
		{"empty UserName", &iam.DetachUserPolicyInput{UserName: aws.String(""), PolicyArn: aws.String(arn)}, awserrors.ErrorMissingParameter},
		{"nil PolicyArn", &iam.DetachUserPolicyInput{UserName: aws.String("alice")}, awserrors.ErrorMissingParameter},
		{"empty PolicyArn", &iam.DetachUserPolicyInput{UserName: aws.String("alice"), PolicyArn: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.DetachUserPolicyInput{UserName: aws.String("alice"), PolicyArn: aws.String(arn)}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DetachUserPolicy(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestListAttachedUserPolicies(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.ListAttachedUserPoliciesInput
		wantErr string
	}{
		{"nil UserName", &iam.ListAttachedUserPoliciesInput{}, awserrors.ErrorMissingParameter},
		{"empty UserName", &iam.ListAttachedUserPoliciesInput{UserName: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.ListAttachedUserPoliciesInput{UserName: aws.String("alice")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ListAttachedUserPolicies(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}
