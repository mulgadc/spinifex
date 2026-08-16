package accountteardown

//test:in-package — the IAM reapers are unexported, and the rules worth testing
// are their delete ordering (detach before delete) and the scoping that keeps
// teardown off resources the account does not own.

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeIAM answers the calls the identity reapers make and records them in
// order. It embeds the interface so the many methods teardown never touches
// stay unimplemented — calling one is a test failure by panic, which is the
// honest outcome for a reaper reaching somewhere it was not meant to.
type fakeIAM struct {
	handlers_iam.IAMService

	calls []string

	users            []string
	roles            []string
	groups           []string
	policies         []string
	instanceProfiles []string
	accessKeys       map[string][]string
	profileRoles     map[string][]string
	attachedUser     []string
	inlineUser       []string
	attachedRole     []string
	inlineRole       []string

	oidcProviders []string

	listUsersErr error
	listKeysErr  error
	scopeAsked   string
}

func newFakeIAM() *fakeIAM {
	return &fakeIAM{accessKeys: map[string][]string{}, profileRoles: map[string][]string{}}
}

func (f *fakeIAM) record(call string) { f.calls = append(f.calls, call) }

func (f *fakeIAM) ListOpenIDConnectProviders(_ string, _ *iam.ListOpenIDConnectProvidersInput) (*iam.ListOpenIDConnectProvidersOutput, error) {
	f.record("ListOpenIDConnectProviders")
	out := &iam.ListOpenIDConnectProvidersOutput{}
	for _, arn := range f.oidcProviders {
		out.OpenIDConnectProviderList = append(out.OpenIDConnectProviderList,
			&iam.OpenIDConnectProviderListEntry{Arn: aws.String(arn)})
	}
	out.OpenIDConnectProviderList = append(out.OpenIDConnectProviderList, nil)
	return out, nil
}

func (f *fakeIAM) DeleteOpenIDConnectProvider(_ string, input *iam.DeleteOpenIDConnectProviderInput) (*iam.DeleteOpenIDConnectProviderOutput, error) {
	f.record("DeleteOpenIDConnectProvider " + aws.StringValue(input.OpenIDConnectProviderArn))
	return &iam.DeleteOpenIDConnectProviderOutput{}, nil
}

func (f *fakeIAM) ListUsers(_ string, _ *iam.ListUsersInput) (*iam.ListUsersOutput, error) {
	f.record("ListUsers")
	if f.listUsersErr != nil {
		return nil, f.listUsersErr
	}
	out := &iam.ListUsersOutput{}
	for _, name := range f.users {
		out.Users = append(out.Users, &iam.User{UserName: aws.String(name)})
	}
	out.Users = append(out.Users, nil)
	return out, nil
}

func (f *fakeIAM) ListAccessKeys(_ string, input *iam.ListAccessKeysInput) (*iam.ListAccessKeysOutput, error) {
	f.record("ListAccessKeys")
	if f.listKeysErr != nil {
		return nil, f.listKeysErr
	}
	out := &iam.ListAccessKeysOutput{}
	for _, key := range f.accessKeys[aws.StringValue(input.UserName)] {
		out.AccessKeyMetadata = append(out.AccessKeyMetadata, &iam.AccessKeyMetadata{
			AccessKeyId: aws.String(key),
		})
	}
	out.AccessKeyMetadata = append(out.AccessKeyMetadata, nil)
	return out, nil
}

func (f *fakeIAM) DeleteAccessKey(_ string, _ *iam.DeleteAccessKeyInput) (*iam.DeleteAccessKeyOutput, error) {
	f.record("DeleteAccessKey")
	return &iam.DeleteAccessKeyOutput{}, nil
}

func (f *fakeIAM) ListAttachedUserPolicies(_ string, _ *iam.ListAttachedUserPoliciesInput) (*iam.ListAttachedUserPoliciesOutput, error) {
	f.record("ListAttachedUserPolicies")
	out := &iam.ListAttachedUserPoliciesOutput{}
	for _, arn := range f.attachedUser {
		out.AttachedPolicies = append(out.AttachedPolicies, &iam.AttachedPolicy{PolicyArn: aws.String(arn)})
	}
	return out, nil
}

func (f *fakeIAM) DetachUserPolicy(_ string, _ *iam.DetachUserPolicyInput) (*iam.DetachUserPolicyOutput, error) {
	f.record("DetachUserPolicy")
	return &iam.DetachUserPolicyOutput{}, nil
}

func (f *fakeIAM) ListUserPolicies(_ string, _ *iam.ListUserPoliciesInput) (*iam.ListUserPoliciesOutput, error) {
	f.record("ListUserPolicies")
	out := &iam.ListUserPoliciesOutput{}
	for _, name := range f.inlineUser {
		out.PolicyNames = append(out.PolicyNames, aws.String(name))
	}
	return out, nil
}

func (f *fakeIAM) DeleteUserPolicy(_ string, _ *iam.DeleteUserPolicyInput) (*iam.DeleteUserPolicyOutput, error) {
	f.record("DeleteUserPolicy")
	return &iam.DeleteUserPolicyOutput{}, nil
}

func (f *fakeIAM) DeleteUser(_ string, _ *iam.DeleteUserInput) (*iam.DeleteUserOutput, error) {
	f.record("DeleteUser")
	return &iam.DeleteUserOutput{}, nil
}

func (f *fakeIAM) ListRoles(_ string, _ *iam.ListRolesInput) (*iam.ListRolesOutput, error) {
	f.record("ListRoles")
	out := &iam.ListRolesOutput{}
	for _, name := range f.roles {
		out.Roles = append(out.Roles, &iam.Role{RoleName: aws.String(name)})
	}
	out.Roles = append(out.Roles, nil)
	return out, nil
}

func (f *fakeIAM) ListAttachedRolePolicies(_ string, _ *iam.ListAttachedRolePoliciesInput) (*iam.ListAttachedRolePoliciesOutput, error) {
	f.record("ListAttachedRolePolicies")
	out := &iam.ListAttachedRolePoliciesOutput{}
	for _, arn := range f.attachedRole {
		out.AttachedPolicies = append(out.AttachedPolicies, &iam.AttachedPolicy{PolicyArn: aws.String(arn)})
	}
	return out, nil
}

func (f *fakeIAM) DetachRolePolicy(_ string, _ *iam.DetachRolePolicyInput) (*iam.DetachRolePolicyOutput, error) {
	f.record("DetachRolePolicy")
	return &iam.DetachRolePolicyOutput{}, nil
}

func (f *fakeIAM) ListRolePolicies(_ string, _ *iam.ListRolePoliciesInput) (*iam.ListRolePoliciesOutput, error) {
	f.record("ListRolePolicies")
	out := &iam.ListRolePoliciesOutput{}
	for _, name := range f.inlineRole {
		out.PolicyNames = append(out.PolicyNames, aws.String(name))
	}
	return out, nil
}

func (f *fakeIAM) DeleteRolePolicy(_ string, _ *iam.DeleteRolePolicyInput) (*iam.DeleteRolePolicyOutput, error) {
	f.record("DeleteRolePolicy")
	return &iam.DeleteRolePolicyOutput{}, nil
}

func (f *fakeIAM) DeleteRole(_ string, _ *iam.DeleteRoleInput) (*iam.DeleteRoleOutput, error) {
	f.record("DeleteRole")
	return &iam.DeleteRoleOutput{}, nil
}

func (f *fakeIAM) ListGroups(_ string, _ *iam.ListGroupsInput) (*iam.ListGroupsOutput, error) {
	f.record("ListGroups")
	out := &iam.ListGroupsOutput{}
	for _, name := range f.groups {
		out.Groups = append(out.Groups, &iam.Group{GroupName: aws.String(name)})
	}
	out.Groups = append(out.Groups, nil)
	return out, nil
}

func (f *fakeIAM) DeleteGroup(_ string, _ *iam.DeleteGroupInput) (*iam.DeleteGroupOutput, error) {
	f.record("DeleteGroup")
	return &iam.DeleteGroupOutput{}, nil
}

func (f *fakeIAM) ListPolicies(_ string, input *iam.ListPoliciesInput) (*iam.ListPoliciesOutput, error) {
	f.record("ListPolicies")
	f.scopeAsked = aws.StringValue(input.Scope)
	out := &iam.ListPoliciesOutput{}
	for _, arn := range f.policies {
		out.Policies = append(out.Policies, &iam.Policy{Arn: aws.String(arn)})
	}
	out.Policies = append(out.Policies, nil)
	return out, nil
}

func (f *fakeIAM) DeletePolicy(_ string, _ *iam.DeletePolicyInput) (*iam.DeletePolicyOutput, error) {
	f.record("DeletePolicy")
	return &iam.DeletePolicyOutput{}, nil
}

func (f *fakeIAM) ListInstanceProfiles(_ string, _ *iam.ListInstanceProfilesInput) (*iam.ListInstanceProfilesOutput, error) {
	f.record("ListInstanceProfiles")
	out := &iam.ListInstanceProfilesOutput{}
	for _, name := range f.instanceProfiles {
		out.InstanceProfiles = append(out.InstanceProfiles, &iam.InstanceProfile{
			InstanceProfileName: aws.String(name),
		})
	}
	out.InstanceProfiles = append(out.InstanceProfiles, nil)
	return out, nil
}

func (f *fakeIAM) GetInstanceProfile(_ string, input *iam.GetInstanceProfileInput) (*iam.GetInstanceProfileOutput, error) {
	f.record("GetInstanceProfile")
	profile := &iam.InstanceProfile{InstanceProfileName: input.InstanceProfileName}
	for _, role := range f.profileRoles[aws.StringValue(input.InstanceProfileName)] {
		profile.Roles = append(profile.Roles, &iam.Role{RoleName: aws.String(role)})
	}
	return &iam.GetInstanceProfileOutput{InstanceProfile: profile}, nil
}

func (f *fakeIAM) RemoveRoleFromInstanceProfile(_ string, _ *iam.RemoveRoleFromInstanceProfileInput) (*iam.RemoveRoleFromInstanceProfileOutput, error) {
	f.record("RemoveRoleFromInstanceProfile")
	return &iam.RemoveRoleFromInstanceProfileOutput{}, nil
}

func (f *fakeIAM) DeleteInstanceProfile(_ string, _ *iam.DeleteInstanceProfileInput) (*iam.DeleteInstanceProfileOutput, error) {
	f.record("DeleteInstanceProfile")
	return &iam.DeleteInstanceProfileOutput{}, nil
}

// Access keys are the second quiesce, so they must be removed before the users
// and roles that own them; instance profiles must be unbound before the roles
// they reference.
func TestIAMReapersAreOrderedForRemoval(t *testing.T) {
	reapers := IAMReapers(newFakeIAM())

	var kinds []string
	for _, reaper := range reapers {
		kinds = append(kinds, reaper.Kind())
	}

	assert.Equal(t, []string{
		"access-key", "instance-profile", "iam-user", "iam-role", "iam-group", "iam-policy",
		"oidc-provider",
	}, kinds)

	// Everything the account authenticates with goes in the identity stage,
	// which is the second quiesce.
	for _, reaper := range reapers[:indexOfKind(reapers, "oidc-provider")] {
		assert.Equal(t, StageIdentity, reaper.Stage(), "%s is a credential", reaper.Kind())
	}

	// An OIDC provider is a trust anchor for an EKS cluster's service
	// accounts, not a credential of this account's, and it outlives the
	// cluster — so it is removed with the other platform leftovers.
	assert.Equal(t, StagePlatform, reapers[indexOfKind(reapers, "oidc-provider")].Stage())
}

// Nothing else in teardown reaches an OIDC provider: EKS DeleteCluster does not
// remove it, so an account would be left holding a trust anchor for a cluster
// that no longer exists.
func TestOIDCProviderReaperListsAndDeletesByARN(t *testing.T) {
	svc := newFakeIAM()
	svc.oidcProviders = []string{
		"arn:aws:iam::000000000042:oidc-provider/oidc.spx.local/id/alpha",
	}

	reaper := &oidcProviderReaper{svc: svc}
	found, err := reaper.List(testCtx(t), "000000000042")
	require.NoError(t, err)
	require.Len(t, found, 1)
	assert.Equal(t, "arn:aws:iam::000000000042:oidc-provider/oidc.spx.local/id/alpha", found[0].ID)

	require.NoError(t, reaper.Delete(testCtx(t), "000000000042", found[0], false))
	assert.Contains(t, svc.calls,
		"DeleteOpenIDConnectProvider arn:aws:iam::000000000042:oidc-provider/oidc.spx.local/id/alpha")
}

// Keys are addressed by user, so a key whose user is missed stays valid — the
// listing has to walk every user rather than a single one.
func TestAccessKeyReaperWalksEveryUser(t *testing.T) {
	svc := newFakeIAM()
	svc.users = []string{"admin", "deploy"}
	svc.accessKeys = map[string][]string{"admin": {"AKIA1"}, "deploy": {"AKIA2", "AKIA3"}}

	found, err := (&accessKeyReaper{svc: svc}).List(testCtx(t), "000000000042")
	require.NoError(t, err)

	require.Len(t, found, 3)
	assert.Equal(t, "AKIA1", found[0].ID)
	// The owning user has to travel with the key: DeleteAccessKey needs both.
	assert.Equal(t, "admin", found[0].Detail)
	assert.Equal(t, "deploy", found[2].Detail)
}

// Reading a failed listing as an empty account is how credentials survive a
// teardown, so both listings fail loudly.
func TestAccessKeyReaperFailsClosed(t *testing.T) {
	svc := newFakeIAM()
	svc.listUsersErr = errors.New("kv unavailable")
	_, err := (&accessKeyReaper{svc: svc}).List(testCtx(t), "000000000042")
	assert.Error(t, err)

	svc = newFakeIAM()
	svc.users = []string{"admin"}
	svc.listKeysErr = errors.New("kv unavailable")
	_, err = (&accessKeyReaper{svc: svc}).List(testCtx(t), "000000000042")
	assert.Error(t, err)
}

// A user with attachments cannot be deleted, so the attachments go first.
func TestUserReaperStripsPoliciesBeforeDeleting(t *testing.T) {
	svc := newFakeIAM()
	svc.attachedUser = []string{"arn:aws:iam::000000000042:policy/admin"}
	svc.inlineUser = []string{"inline-1"}

	err := (&iamUserReaper{svc: svc}).Delete(testCtx(t), "000000000042",
		Resource{Kind: "iam-user", ID: "admin"}, false)
	require.NoError(t, err)

	assert.Equal(t, []string{
		"ListAttachedUserPolicies", "DetachUserPolicy",
		"ListUserPolicies", "DeleteUserPolicy",
		"DeleteUser",
	}, svc.calls)
}

func TestRoleReaperStripsPoliciesBeforeDeleting(t *testing.T) {
	svc := newFakeIAM()
	svc.attachedRole = []string{"arn:aws:iam::000000000042:policy/task"}
	svc.inlineRole = []string{"inline-1"}

	err := (&iamRoleReaper{svc: svc}).Delete(testCtx(t), "000000000042",
		Resource{Kind: "iam-role", ID: "task-role"}, false)
	require.NoError(t, err)

	assert.Equal(t, []string{
		"ListAttachedRolePolicies", "DetachRolePolicy",
		"ListRolePolicies", "DeleteRolePolicy",
		"DeleteRole",
	}, svc.calls)
}

// A profile still referencing a role leaves the role reaper with a role it
// cannot delete, which reads as a stuck resource for an unrelated reason.
func TestInstanceProfileReaperUnbindsItsRoles(t *testing.T) {
	svc := newFakeIAM()
	svc.instanceProfiles = []string{"web"}
	svc.profileRoles = map[string][]string{"web": {"web-role"}}

	reaper := &instanceProfileReaper{svc: svc}
	found, err := reaper.List(testCtx(t), "000000000042")
	require.NoError(t, err)
	require.Len(t, found, 1)

	require.NoError(t, reaper.Delete(testCtx(t), "000000000042", found[0], false))
	assert.Equal(t, []string{
		"ListInstanceProfiles", "GetInstanceProfile",
		"RemoveRoleFromInstanceProfile", "DeleteInstanceProfile",
	}, svc.calls)
}

// AWS-managed policies are shared with every other tenant and are not the
// account's to delete.
func TestPolicyReaperAsksOnlyForLocalPolicies(t *testing.T) {
	svc := newFakeIAM()
	svc.policies = []string{"arn:aws:iam::000000000042:policy/app"}

	reaper := &iamPolicyReaper{svc: svc}
	found, err := reaper.List(testCtx(t), "000000000042")
	require.NoError(t, err)

	assert.Equal(t, "Local", svc.scopeAsked)
	require.Len(t, found, 1)
	assert.Equal(t, "arn:aws:iam::000000000042:policy/app", found[0].ID)
	assert.NoError(t, reaper.Delete(testCtx(t), "000000000042", found[0], false))
}

func TestUserRoleAndGroupListingsSkipEmptyRecords(t *testing.T) {
	svc := newFakeIAM()
	svc.users = []string{"admin"}
	svc.roles = []string{"task-role"}
	svc.groups = []string{"engineers"}

	ctx := testCtx(t)
	users, err := (&iamUserReaper{svc: svc}).List(ctx, "000000000042")
	require.NoError(t, err)
	assert.Len(t, users, 1)

	roles, err := (&iamRoleReaper{svc: svc}).List(ctx, "000000000042")
	require.NoError(t, err)
	assert.Len(t, roles, 1)

	groupReaper := &iamGroupReaper{svc: svc}
	groups, err := groupReaper.List(ctx, "000000000042")
	require.NoError(t, err)
	require.Len(t, groups, 1)
	assert.NoError(t, groupReaper.Delete(ctx, "000000000042", groups[0], false))
}

// A key that is already gone is a success: teardown re-runs after a crash and
// would otherwise never finish what the first pass started.
func TestIdentityDeletesTolerateMissingRecords(t *testing.T) {
	svc := newFakeIAM()

	assert.NoError(t, (&accessKeyReaper{svc: svc}).Delete(testCtx(t), "000000000042",
		Resource{Kind: "access-key", ID: "AKIA1", Detail: "admin"}, false))
}
