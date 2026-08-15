package accountteardown

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
)

// IAMReapers returns the identity-stage reapers in teardown order.
//
// Access keys go first and deliberately: it is the second quiesce, and unlike
// the account-status gate it holds even against a caller that reached the
// cluster some other way.
func IAMReapers(svc handlers_iam.IAMService) []Reaper {
	return []Reaper{
		&accessKeyReaper{svc: svc},
		&instanceProfileReaper{svc: svc},
		&iamUserReaper{svc: svc},
		&iamRoleReaper{svc: svc},
		&iamGroupReaper{svc: svc},
		&iamPolicyReaper{svc: svc},
	}
}

type accessKeyReaper struct{ svc handlers_iam.IAMService }

func (r *accessKeyReaper) Kind() string { return "access-key" }
func (r *accessKeyReaper) Stage() Stage { return StageIdentity }

// List walks every user because access keys are addressed by user, and a key
// whose user is missed stays valid.
func (r *accessKeyReaper) List(_ context.Context, accountID string) ([]Resource, error) {
	users, err := r.svc.ListUsers(accountID, &iam.ListUsersInput{})
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}

	var found []Resource
	for _, user := range users.Users {
		if user == nil || user.UserName == nil {
			continue
		}
		keys, err := r.svc.ListAccessKeys(accountID, &iam.ListAccessKeysInput{UserName: user.UserName})
		if err != nil {
			return nil, fmt.Errorf("list access keys for %s: %w", *user.UserName, err)
		}
		for _, key := range keys.AccessKeyMetadata {
			if key == nil || key.AccessKeyId == nil {
				continue
			}
			found = append(found, Resource{Kind: r.Kind(), ID: *key.AccessKeyId, Detail: *user.UserName})
		}
	}
	return found, nil
}

func (r *accessKeyReaper) Delete(_ context.Context, accountID string, resource Resource, _ bool) error {
	_, err := r.svc.DeleteAccessKey(accountID, &iam.DeleteAccessKeyInput{
		AccessKeyId: aws.String(resource.ID),
		UserName:    aws.String(resource.Detail),
	})
	return ignoreAlreadyGone(err)
}

type iamUserReaper struct{ svc handlers_iam.IAMService }

func (r *iamUserReaper) Kind() string { return "iam-user" }
func (r *iamUserReaper) Stage() Stage { return StageIdentity }

func (r *iamUserReaper) List(_ context.Context, accountID string) ([]Resource, error) {
	users, err := r.svc.ListUsers(accountID, &iam.ListUsersInput{})
	if err != nil {
		return nil, err
	}

	var found []Resource
	for _, user := range users.Users {
		if user == nil || user.UserName == nil {
			continue
		}
		found = append(found, Resource{Kind: r.Kind(), ID: *user.UserName})
	}
	return found, nil
}

// Delete strips the user's policies first: AWS refuses to delete a user that
// still has attachments, and so does this implementation.
func (r *iamUserReaper) Delete(_ context.Context, accountID string, resource Resource, _ bool) error {
	attached, err := r.svc.ListAttachedUserPolicies(accountID, &iam.ListAttachedUserPoliciesInput{
		UserName: aws.String(resource.ID),
	})
	if err != nil && !isAlreadyGone(err) {
		return err
	}
	if attached != nil {
		for _, policy := range attached.AttachedPolicies {
			if policy == nil || policy.PolicyArn == nil {
				continue
			}
			if _, err := r.svc.DetachUserPolicy(accountID, &iam.DetachUserPolicyInput{
				UserName: aws.String(resource.ID), PolicyArn: policy.PolicyArn,
			}); err != nil && !isAlreadyGone(err) {
				return err
			}
		}
	}

	inline, err := r.svc.ListUserPolicies(accountID, &iam.ListUserPoliciesInput{UserName: aws.String(resource.ID)})
	if err != nil && !isAlreadyGone(err) {
		return err
	}
	if inline != nil {
		for _, name := range inline.PolicyNames {
			if name == nil {
				continue
			}
			if _, err := r.svc.DeleteUserPolicy(accountID, &iam.DeleteUserPolicyInput{
				UserName: aws.String(resource.ID), PolicyName: name,
			}); err != nil && !isAlreadyGone(err) {
				return err
			}
		}
	}

	_, err = r.svc.DeleteUser(accountID, &iam.DeleteUserInput{UserName: aws.String(resource.ID)})
	return ignoreAlreadyGone(err)
}

type iamRoleReaper struct{ svc handlers_iam.IAMService }

func (r *iamRoleReaper) Kind() string { return "iam-role" }
func (r *iamRoleReaper) Stage() Stage { return StageIdentity }

func (r *iamRoleReaper) List(_ context.Context, accountID string) ([]Resource, error) {
	roles, err := r.svc.ListRoles(accountID, &iam.ListRolesInput{})
	if err != nil {
		return nil, err
	}

	var found []Resource
	for _, role := range roles.Roles {
		if role == nil || role.RoleName == nil {
			continue
		}
		found = append(found, Resource{Kind: r.Kind(), ID: *role.RoleName})
	}
	return found, nil
}

func (r *iamRoleReaper) Delete(_ context.Context, accountID string, resource Resource, _ bool) error {
	attached, err := r.svc.ListAttachedRolePolicies(accountID, &iam.ListAttachedRolePoliciesInput{
		RoleName: aws.String(resource.ID),
	})
	if err != nil && !isAlreadyGone(err) {
		return err
	}
	if attached != nil {
		for _, policy := range attached.AttachedPolicies {
			if policy == nil || policy.PolicyArn == nil {
				continue
			}
			if _, err := r.svc.DetachRolePolicy(accountID, &iam.DetachRolePolicyInput{
				RoleName: aws.String(resource.ID), PolicyArn: policy.PolicyArn,
			}); err != nil && !isAlreadyGone(err) {
				return err
			}
		}
	}

	inline, err := r.svc.ListRolePolicies(accountID, &iam.ListRolePoliciesInput{RoleName: aws.String(resource.ID)})
	if err != nil && !isAlreadyGone(err) {
		return err
	}
	if inline != nil {
		for _, name := range inline.PolicyNames {
			if name == nil {
				continue
			}
			if _, err := r.svc.DeleteRolePolicy(accountID, &iam.DeleteRolePolicyInput{
				RoleName: aws.String(resource.ID), PolicyName: name,
			}); err != nil && !isAlreadyGone(err) {
				return err
			}
		}
	}

	_, err = r.svc.DeleteRole(accountID, &iam.DeleteRoleInput{RoleName: aws.String(resource.ID)})
	return ignoreAlreadyGone(err)
}

type iamGroupReaper struct{ svc handlers_iam.IAMService }

func (r *iamGroupReaper) Kind() string { return "iam-group" }
func (r *iamGroupReaper) Stage() Stage { return StageIdentity }

func (r *iamGroupReaper) List(_ context.Context, accountID string) ([]Resource, error) {
	groups, err := r.svc.ListGroups(accountID, &iam.ListGroupsInput{})
	if err != nil {
		return nil, err
	}

	var found []Resource
	for _, group := range groups.Groups {
		if group == nil || group.GroupName == nil {
			continue
		}
		found = append(found, Resource{Kind: r.Kind(), ID: *group.GroupName})
	}
	return found, nil
}

func (r *iamGroupReaper) Delete(_ context.Context, accountID string, resource Resource, _ bool) error {
	_, err := r.svc.DeleteGroup(accountID, &iam.DeleteGroupInput{GroupName: aws.String(resource.ID)})
	return ignoreAlreadyGone(err)
}

type iamPolicyReaper struct{ svc handlers_iam.IAMService }

func (r *iamPolicyReaper) Kind() string { return "iam-policy" }
func (r *iamPolicyReaper) Stage() Stage { return StageIdentity }

// List is scoped to the account's own policies. AWS-managed policies are not
// the account's to delete and are shared with every other tenant.
func (r *iamPolicyReaper) List(_ context.Context, accountID string) ([]Resource, error) {
	policies, err := r.svc.ListPolicies(accountID, &iam.ListPoliciesInput{Scope: aws.String("Local")})
	if err != nil {
		return nil, err
	}

	var found []Resource
	for _, policy := range policies.Policies {
		if policy == nil || policy.Arn == nil {
			continue
		}
		found = append(found, Resource{Kind: r.Kind(), ID: *policy.Arn})
	}
	return found, nil
}

func (r *iamPolicyReaper) Delete(_ context.Context, accountID string, resource Resource, _ bool) error {
	_, err := r.svc.DeletePolicy(accountID, &iam.DeletePolicyInput{PolicyArn: aws.String(resource.ID)})
	return ignoreAlreadyGone(err)
}

type instanceProfileReaper struct{ svc handlers_iam.IAMService }

func (r *instanceProfileReaper) Kind() string { return "instance-profile" }
func (r *instanceProfileReaper) Stage() Stage { return StageIdentity }

func (r *instanceProfileReaper) List(_ context.Context, accountID string) ([]Resource, error) {
	profiles, err := r.svc.ListInstanceProfiles(accountID, &iam.ListInstanceProfilesInput{})
	if err != nil {
		return nil, err
	}

	var found []Resource
	for _, profile := range profiles.InstanceProfiles {
		if profile == nil || profile.InstanceProfileName == nil {
			continue
		}
		found = append(found, Resource{Kind: r.Kind(), ID: *profile.InstanceProfileName})
	}
	return found, nil
}

// Delete unbinds the profile's roles first, otherwise the role reaper later
// finds a role it cannot delete because a profile still references it.
func (r *instanceProfileReaper) Delete(_ context.Context, accountID string, resource Resource, _ bool) error {
	profile, err := r.svc.GetInstanceProfile(accountID, &iam.GetInstanceProfileInput{
		InstanceProfileName: aws.String(resource.ID),
	})
	if err != nil && !isAlreadyGone(err) {
		return err
	}
	if profile != nil && profile.InstanceProfile != nil {
		for _, role := range profile.InstanceProfile.Roles {
			if role == nil || role.RoleName == nil {
				continue
			}
			if _, err := r.svc.RemoveRoleFromInstanceProfile(accountID, &iam.RemoveRoleFromInstanceProfileInput{
				InstanceProfileName: aws.String(resource.ID), RoleName: role.RoleName,
			}); err != nil && !isAlreadyGone(err) {
				return err
			}
		}
	}

	_, err = r.svc.DeleteInstanceProfile(accountID, &iam.DeleteInstanceProfileInput{
		InstanceProfileName: aws.String(resource.ID),
	})
	return ignoreAlreadyGone(err)
}
