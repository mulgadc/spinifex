package handlers_iam

import (
	"github.com/aws/aws-sdk-go/service/iam"
)

// IAMService defines the interface for IAM operations.
type IAMService interface {
	// User CRUD — account-scoped
	CreateUser(accountID string, input *iam.CreateUserInput) (*iam.CreateUserOutput, error)
	GetUser(accountID string, input *iam.GetUserInput) (*iam.GetUserOutput, error)
	ListUsers(accountID string, input *iam.ListUsersInput) (*iam.ListUsersOutput, error)
	DeleteUser(accountID string, input *iam.DeleteUserInput) (*iam.DeleteUserOutput, error)

	// Access key lifecycle — account-scoped
	CreateAccessKey(accountID string, input *iam.CreateAccessKeyInput) (*iam.CreateAccessKeyOutput, error)
	ListAccessKeys(accountID string, input *iam.ListAccessKeysInput) (*iam.ListAccessKeysOutput, error)
	DeleteAccessKey(accountID string, input *iam.DeleteAccessKeyInput) (*iam.DeleteAccessKeyOutput, error)
	UpdateAccessKey(accountID string, input *iam.UpdateAccessKeyInput) (*iam.UpdateAccessKeyOutput, error)

	// Policy CRUD — account-scoped
	CreatePolicy(accountID string, input *iam.CreatePolicyInput) (*iam.CreatePolicyOutput, error)
	GetPolicy(accountID string, input *iam.GetPolicyInput) (*iam.GetPolicyOutput, error)
	GetPolicyVersion(accountID string, input *iam.GetPolicyVersionInput) (*iam.GetPolicyVersionOutput, error)
	ListPolicyVersions(accountID string, input *iam.ListPolicyVersionsInput) (*iam.ListPolicyVersionsOutput, error)
	ListPolicies(accountID string, input *iam.ListPoliciesInput) (*iam.ListPoliciesOutput, error)
	DeletePolicy(accountID string, input *iam.DeletePolicyInput) (*iam.DeletePolicyOutput, error)

	// Policy attachment — account-scoped
	AttachUserPolicy(accountID string, input *iam.AttachUserPolicyInput) (*iam.AttachUserPolicyOutput, error)
	DetachUserPolicy(accountID string, input *iam.DetachUserPolicyInput) (*iam.DetachUserPolicyOutput, error)
	ListAttachedUserPolicies(accountID string, input *iam.ListAttachedUserPoliciesInput) (*iam.ListAttachedUserPoliciesOutput, error)

	// Role CRUD — account-scoped
	CreateRole(accountID string, input *iam.CreateRoleInput) (*iam.CreateRoleOutput, error)
	GetRole(accountID string, input *iam.GetRoleInput) (*iam.GetRoleOutput, error)
	ListRoles(accountID string, input *iam.ListRolesInput) (*iam.ListRolesOutput, error)
	DeleteRole(accountID string, input *iam.DeleteRoleInput) (*iam.DeleteRoleOutput, error)
	UpdateRole(accountID string, input *iam.UpdateRoleInput) (*iam.UpdateRoleOutput, error)
	UpdateAssumeRolePolicy(accountID string, input *iam.UpdateAssumeRolePolicyInput) (*iam.UpdateAssumeRolePolicyOutput, error)

	// Role policy attachment — account-scoped
	AttachRolePolicy(accountID string, input *iam.AttachRolePolicyInput) (*iam.AttachRolePolicyOutput, error)
	DetachRolePolicy(accountID string, input *iam.DetachRolePolicyInput) (*iam.DetachRolePolicyOutput, error)
	ListAttachedRolePolicies(accountID string, input *iam.ListAttachedRolePoliciesInput) (*iam.ListAttachedRolePoliciesOutput, error)

	// Instance profile CRUD — account-scoped
	CreateInstanceProfile(accountID string, input *iam.CreateInstanceProfileInput) (*iam.CreateInstanceProfileOutput, error)
	GetInstanceProfile(accountID string, input *iam.GetInstanceProfileInput) (*iam.GetInstanceProfileOutput, error)
	ListInstanceProfiles(accountID string, input *iam.ListInstanceProfilesInput) (*iam.ListInstanceProfilesOutput, error)
	DeleteInstanceProfile(accountID string, input *iam.DeleteInstanceProfileInput) (*iam.DeleteInstanceProfileOutput, error)
	ListInstanceProfilesForRole(accountID string, input *iam.ListInstanceProfilesForRoleInput) (*iam.ListInstanceProfilesForRoleOutput, error)

	// Instance profile ↔ role binding — account-scoped
	AddRoleToInstanceProfile(accountID string, input *iam.AddRoleToInstanceProfileInput) (*iam.AddRoleToInstanceProfileOutput, error)
	RemoveRoleFromInstanceProfile(accountID string, input *iam.RemoveRoleFromInstanceProfileInput) (*iam.RemoveRoleFromInstanceProfileOutput, error)

	// OIDC identity-provider registry — account-scoped. Registers a cluster
	// issuer as a trusted federated IdP so STS AssumeRoleWithWebIdentity will
	// honour tokens it signs (IRSA).
	CreateOpenIDConnectProvider(accountID string, input *iam.CreateOpenIDConnectProviderInput) (*iam.CreateOpenIDConnectProviderOutput, error)
	GetOpenIDConnectProvider(accountID string, input *iam.GetOpenIDConnectProviderInput) (*iam.GetOpenIDConnectProviderOutput, error)
	ListOpenIDConnectProviders(accountID string, input *iam.ListOpenIDConnectProvidersInput) (*iam.ListOpenIDConnectProvidersOutput, error)
	DeleteOpenIDConnectProvider(accountID string, input *iam.DeleteOpenIDConnectProviderInput) (*iam.DeleteOpenIDConnectProviderOutput, error)

	// ResolveInstanceProfile dereferences a RunInstancesInput.IamInstanceProfile
	// reference (name or ARN) to the canonical InstanceProfile record. Used by
	// EC2 paths only. Cross-account ARNs are rejected as a defence-in-depth
	// check; the gateway also enforces this and returns AccessDenied.
	ResolveInstanceProfile(accountID, nameOrARN string) (*InstanceProfile, error)

	// Policy evaluation (internal — used by gateway enforcement)
	GetUserPolicies(accountID, userName string) ([]PolicyDocument, error)

	// Auth (internal — used by SigV4 middleware and bootstrap, not exposed via gateway)
	LookupAccessKey(accessKeyID string) (*AccessKey, error)
	DecryptSecret(ciphertext string) (string, error)
	SeedBootstrap(data *BootstrapData) error
	IsEmpty() (bool, error)

	// Account operations
	CreateAccount(name string) (*Account, error)
	GetAccount(accountID string) (*Account, error)
	ListAccounts() ([]*Account, error)
}
