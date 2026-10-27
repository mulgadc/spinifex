package iam

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/nats-io/nats.go/jetstream"

	iamarn "github.com/mulgadc/bluebottle/pkg/auth"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
)

// maxGroupsPerUser caps how many groups a single user may belong to. Mirrors the
// hardcoded maxAccessKeysPerUser limit and bounds the per-request getGroup
// fan-out that GetUserPolicies performs on the authorization hot path.
const maxGroupsPerUser = 10

func (s *IAMServiceImpl) CreateGroup(accountID string, input *iam.CreateGroupInput) (*iam.CreateGroupOutput, error) {
	ctx := context.Background()
	groupName := *input.GroupName
	if err := validateIAMName("groupName", groupName, 128); err != nil {
		return nil, err
	}

	path := "/"
	if input.Path != nil {
		path = *input.Path
		if err := validatePath(path); err != nil {
			return nil, err
		}
	}

	groupID, err := generateIAMID("AGPA")
	if err != nil {
		return nil, fmt.Errorf("generate group ID: %w", err)
	}

	group := Group{
		GroupName:        groupName,
		GroupID:          groupID,
		AccountID:        accountID,
		ARN:              arn.FormatIAMPath(arn.IAMGroup, accountID, path, groupName),
		Path:             path,
		CreatedAt:        time.Now().UTC().Format(time.RFC3339),
		AttachedPolicies: []string{},
		Tags:             []Tag{},
	}

	data, err := json.Marshal(group)
	if err != nil {
		return nil, fmt.Errorf("marshal group: %w", err)
	}

	kvKey := accountID + "." + groupName
	if _, err := s.groupsBucket.Create(ctx, kvKey, data); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return nil, errors.New(awserrors.ErrorIAMEntityAlreadyExists)
		}
		return nil, fmt.Errorf("store group: %w", err)
	}

	slog.Info("IAM group created", "accountID", accountID, "groupName", groupName, "groupID", group.GroupID)

	return &iam.CreateGroupOutput{Group: groupToSDK(&group)}, nil
}

func (s *IAMServiceImpl) GetGroup(accountID string, input *iam.GetGroupInput) (*iam.GetGroupOutput, error) {
	ctx := context.Background()
	group, err := s.getGroup(ctx, accountID, *input.GroupName)
	if err != nil {
		return nil, err
	}

	members, err := s.findGroupMembers(ctx, accountID, group.GroupName)
	if err != nil {
		return nil, fmt.Errorf("find group members: %w", err)
	}

	// GetGroupOutput.Users is a required SDK field — return an empty slice, never nil.
	users := make([]*iam.User, 0, len(members))
	for _, u := range members {
		users = append(users, &iam.User{
			UserName:   aws.String(u.UserName),
			UserId:     aws.String(u.UserID),
			Arn:        aws.String(u.ARN),
			Path:       aws.String(u.Path),
			CreateDate: aws.Time(parseCreatedAt(u.CreatedAt)),
		})
	}

	return &iam.GetGroupOutput{
		Group:       groupToSDK(group),
		Users:       users,
		IsTruncated: aws.Bool(false),
	}, nil
}

func (s *IAMServiceImpl) ListGroups(accountID string, input *iam.ListGroupsInput) (*iam.ListGroupsOutput, error) {
	ctx := context.Background()
	keys, err := kvutil.Keys(ctx, s.groupsBucket)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return &iam.ListGroupsOutput{
				Groups:      []*iam.Group{},
				IsTruncated: aws.Bool(false),
			}, nil
		}
		return nil, fmt.Errorf("list group keys: %w", err)
	}

	pathPrefix := "/"
	if input.PathPrefix != nil {
		pathPrefix = *input.PathPrefix
	}

	keyPrefix := accountID + "."
	// Non-nil: Groups is a required member, and a nil slice marshals to no
	// element at all rather than an empty one.
	groups := []*iam.Group{}
	for _, key := range keys {
		if key == kvutil.VersionKey {
			continue
		}
		if !strings.HasPrefix(key, keyPrefix) {
			continue
		}

		entry, err := s.groupsBucket.Get(ctx, key)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				slog.Debug("ListGroups: group key disappeared (concurrent delete)", "key", key)
			} else {
				slog.Warn("ListGroups: failed to get group", "key", key, "err", err)
			}
			continue
		}

		var group Group
		if err := json.Unmarshal(entry.Value(), &group); err != nil {
			slog.Warn("ListGroups: failed to unmarshal group", "key", key, "err", err)
			continue
		}

		if !strings.HasPrefix(group.Path, pathPrefix) {
			continue
		}

		groups = append(groups, groupToSDK(&group))
	}

	return &iam.ListGroupsOutput{
		Groups:      groups,
		IsTruncated: aws.Bool(false),
	}, nil
}

func (s *IAMServiceImpl) DeleteGroup(accountID string, input *iam.DeleteGroupInput) (*iam.DeleteGroupOutput, error) {
	ctx := context.Background()
	groupName := *input.GroupName

	group, err := s.getGroup(ctx, accountID, groupName)
	if err != nil {
		return nil, err
	}

	if len(group.AttachedPolicies) > 0 {
		return nil, errors.New(awserrors.ErrorIAMDeleteConflict)
	}

	if len(group.InlinePolicies) > 0 {
		return nil, errors.New(awserrors.ErrorIAMDeleteConflict)
	}

	members, err := s.findGroupMembers(ctx, accountID, groupName)
	if err != nil {
		return nil, fmt.Errorf("check group members: %w", err)
	}
	if len(members) > 0 {
		return nil, errors.New(awserrors.ErrorIAMDeleteConflict)
	}

	if err := s.groupsBucket.Delete(ctx, accountID+"."+groupName); err != nil {
		return nil, fmt.Errorf("delete group: %w", err)
	}

	slog.Info("IAM group deleted", "accountID", accountID, "groupName", groupName)
	return &iam.DeleteGroupOutput{}, nil
}

// ---------------------------------------------------------------------------
// Group membership — canonical on the User record (User.Groups)
// ---------------------------------------------------------------------------

func (s *IAMServiceImpl) AddUserToGroup(accountID string, input *iam.AddUserToGroupInput) (*iam.AddUserToGroupOutput, error) {
	ctx := context.Background()
	groupName := *input.GroupName
	userName := *input.UserName

	if _, err := s.getGroup(ctx, accountID, groupName); err != nil {
		return nil, err
	}

	err := s.updateUserCAS(ctx, accountID, userName, func(u *User) (bool, error) {
		if slices.Contains(u.Groups, groupName) { // idempotent
			return false, nil
		}
		if len(u.Groups) >= maxGroupsPerUser {
			return false, errors.New(awserrors.ErrorIAMLimitExceeded)
		}
		u.Groups = append(u.Groups, groupName)
		return true, nil
	})
	if err != nil {
		return nil, err
	}

	slog.Info("IAM user added to group", "accountID", accountID, "userName", userName, "groupName", groupName)
	return &iam.AddUserToGroupOutput{}, nil
}

func (s *IAMServiceImpl) RemoveUserFromGroup(accountID string, input *iam.RemoveUserFromGroupInput) (*iam.RemoveUserFromGroupOutput, error) {
	ctx := context.Background()
	groupName := *input.GroupName
	userName := *input.UserName

	// Operate purely on the membership reference so a dangling pointer to an
	// already-deleted group is still cleanable; never fetch the group here.
	err := s.updateUserCAS(ctx, accountID, userName, func(u *User) (bool, error) {
		idx := slices.Index(u.Groups, groupName)
		if idx < 0 {
			return false, errors.New(awserrors.ErrorIAMNoSuchEntity)
		}
		u.Groups = slices.Delete(u.Groups, idx, idx+1)
		return true, nil
	})
	if err != nil {
		return nil, err
	}

	slog.Info("IAM user removed from group", "accountID", accountID, "userName", userName, "groupName", groupName)
	return &iam.RemoveUserFromGroupOutput{}, nil
}

func (s *IAMServiceImpl) ListGroupsForUser(accountID string, input *iam.ListGroupsForUserInput) (*iam.ListGroupsForUserOutput, error) {
	ctx := context.Background()
	user, err := s.getUser(ctx, accountID, *input.UserName)
	if err != nil {
		return nil, err
	}

	// Non-nil: Groups is a required member, and a nil slice marshals to no
	// element at all rather than an empty one.
	groups := []*iam.Group{}
	for _, name := range user.Groups {
		group, err := s.getGroup(ctx, accountID, name)
		if err != nil {
			if err.Error() == awserrors.ErrorIAMNoSuchEntity {
				// A dangling pointer to a vanished group is inert; skip it.
				// Mirrors GetUserPolicies — transient/corrupt errors fail closed.
				slog.Warn("ListGroupsForUser: member references missing group; skipping",
					"accountID", accountID, "user", *input.UserName, "group", name)
				continue
			}
			return nil, err
		}
		groups = append(groups, groupToSDK(group))
	}

	return &iam.ListGroupsForUserOutput{
		Groups:      groups,
		IsTruncated: aws.Bool(false),
	}, nil
}

// ---------------------------------------------------------------------------
// Group policy attachment
// ---------------------------------------------------------------------------

func (s *IAMServiceImpl) AttachGroupPolicy(accountID string, input *iam.AttachGroupPolicyInput) (*iam.AttachGroupPolicyOutput, error) {
	ctx := context.Background()
	groupName := *input.GroupName
	policyARN := *input.PolicyArn

	// AWS-managed ARNs are stored opaquely (like roles); customer-managed ARNs must exist.
	if !iamarn.IsAWSManagedPolicyARN(policyARN) {
		if _, err := s.getPolicyByARN(ctx, accountID, policyARN); err != nil {
			return nil, err
		}
	}

	err := s.updateGroupCAS(ctx, accountID, groupName, func(g *Group) (bool, error) {
		if slices.Contains(g.AttachedPolicies, policyARN) { // idempotent
			return false, nil
		}
		g.AttachedPolicies = append(g.AttachedPolicies, policyARN)
		return true, nil
	})
	if err != nil {
		return nil, err
	}

	slog.Info("IAM policy attached to group", "accountID", accountID, "groupName", groupName, "policyArn", policyARN)
	return &iam.AttachGroupPolicyOutput{}, nil
}

func (s *IAMServiceImpl) DetachGroupPolicy(accountID string, input *iam.DetachGroupPolicyInput) (*iam.DetachGroupPolicyOutput, error) {
	ctx := context.Background()
	groupName := *input.GroupName
	policyARN := *input.PolicyArn

	err := s.updateGroupCAS(ctx, accountID, groupName, func(g *Group) (bool, error) {
		idx := slices.Index(g.AttachedPolicies, policyARN)
		if idx < 0 {
			return false, errors.New(awserrors.ErrorIAMNoSuchEntity)
		}
		g.AttachedPolicies = slices.Delete(g.AttachedPolicies, idx, idx+1)
		return true, nil
	})
	if err != nil {
		return nil, err
	}

	slog.Info("IAM policy detached from group", "accountID", accountID, "groupName", groupName, "policyArn", policyARN)
	return &iam.DetachGroupPolicyOutput{}, nil
}

func (s *IAMServiceImpl) ListAttachedGroupPolicies(accountID string, input *iam.ListAttachedGroupPoliciesInput) (*iam.ListAttachedGroupPoliciesOutput, error) {
	ctx := context.Background()
	group, err := s.getGroup(ctx, accountID, *input.GroupName)
	if err != nil {
		return nil, err
	}

	attached, err := s.attachedPolicies(ctx, accountID, group.AttachedPolicies, aws.StringValue(input.PathPrefix))
	if err != nil {
		return nil, err
	}

	return &iam.ListAttachedGroupPoliciesOutput{
		AttachedPolicies: attached,
		IsTruncated:      aws.Bool(false),
	}, nil
}

// ---------------------------------------------------------------------------
// Group inline policies
// ---------------------------------------------------------------------------

// PutGroupPolicy embeds an inline policy document in a group, keyed by PolicyName.
// Idempotent upsert: a same-name policy is overwritten, mirroring AWS.
func (s *IAMServiceImpl) PutGroupPolicy(accountID string, input *iam.PutGroupPolicyInput) (*iam.PutGroupPolicyOutput, error) {
	ctx := context.Background()
	groupName := *input.GroupName
	policyName := *input.PolicyName
	policyDoc := *input.PolicyDocument

	if err := validateIAMName("policyName", policyName, 128); err != nil {
		return nil, err
	}
	if err := checkPolicyDocumentLength(policyDoc); err != nil {
		return nil, err
	}
	if _, err := ValidatePolicyDocument(policyDoc); err != nil {
		return nil, awserrors.Errorf(awserrors.ErrorIAMMalformedPolicyDocument,
			"policy %q on group %q: %w", policyName, groupName, err)
	}

	err := s.updateGroupCAS(ctx, accountID, groupName, func(g *Group) (bool, error) {
		if err := checkInlinePolicySize("group", groupName, groupPolicySizeQuota, g.InlinePolicies, policyName, policyDoc); err != nil {
			return false, err
		}
		if g.InlinePolicies == nil {
			g.InlinePolicies = map[string]string{}
		}
		g.InlinePolicies[policyName] = policyDoc
		return true, nil
	})
	if err != nil {
		return nil, err
	}

	slog.Info("IAM inline policy put on group", "accountID", accountID, "groupName", groupName, "policyName", policyName)
	return &iam.PutGroupPolicyOutput{}, nil
}

// GetGroupPolicy returns a group's inline policy document by name as a raw JSON
// string, matching the in-repo convention used by GetRolePolicy.
func (s *IAMServiceImpl) GetGroupPolicy(accountID string, input *iam.GetGroupPolicyInput) (*iam.GetGroupPolicyOutput, error) {
	ctx := context.Background()
	groupName := *input.GroupName
	policyName := *input.PolicyName

	group, err := s.getGroup(ctx, accountID, groupName)
	if err != nil {
		return nil, err
	}

	doc, ok := group.InlinePolicies[policyName]
	if !ok {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}

	return &iam.GetGroupPolicyOutput{
		GroupName:      aws.String(groupName),
		PolicyName:     aws.String(policyName),
		PolicyDocument: aws.String(doc),
	}, nil
}

// DeleteGroupPolicy removes a group's inline policy by name. A missing name
// yields NoSuchEntity, matching AWS.
func (s *IAMServiceImpl) DeleteGroupPolicy(accountID string, input *iam.DeleteGroupPolicyInput) (*iam.DeleteGroupPolicyOutput, error) {
	ctx := context.Background()
	groupName := *input.GroupName
	policyName := *input.PolicyName

	err := s.updateGroupCAS(ctx, accountID, groupName, func(g *Group) (bool, error) {
		if _, ok := g.InlinePolicies[policyName]; !ok {
			return false, errors.New(awserrors.ErrorIAMNoSuchEntity)
		}
		delete(g.InlinePolicies, policyName)
		return true, nil
	})
	if err != nil {
		return nil, err
	}

	slog.Info("IAM inline policy deleted from group", "accountID", accountID, "groupName", groupName, "policyName", policyName)
	return &iam.DeleteGroupPolicyOutput{}, nil
}

// ListGroupPolicies returns the names of a group's inline policies, sorted for
// deterministic output. Returns the whole list; the gateway pages it.
func (s *IAMServiceImpl) ListGroupPolicies(accountID string, input *iam.ListGroupPoliciesInput) (*iam.ListGroupPoliciesOutput, error) {
	ctx := context.Background()
	group, err := s.getGroup(ctx, accountID, *input.GroupName)
	if err != nil {
		return nil, err
	}

	rawNames := slices.Sorted(maps.Keys(group.InlinePolicies))

	names := make([]*string, 0, len(rawNames))
	for _, name := range rawNames {
		names = append(names, aws.String(name))
	}

	return &iam.ListGroupPoliciesOutput{
		PolicyNames: names,
		IsTruncated: aws.Bool(false),
	}, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (s *IAMServiceImpl) getGroup(ctx context.Context, accountID, groupName string) (*Group, error) {
	entry, err := s.groupsBucket.Get(ctx, accountID+"."+groupName)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
		}
		return nil, fmt.Errorf("get group: %w", err)
	}

	var group Group
	if err := json.Unmarshal(entry.Value(), &group); err != nil {
		return nil, fmt.Errorf("unmarshal group: %w", err)
	}
	return &group, nil
}

// updateGroupCAS applies mutate to a group under optimistic concurrency, like
// updateUserCAS.
func (s *IAMServiceImpl) updateGroupCAS(ctx context.Context, accountID, groupName string, mutate func(*Group) (bool, error)) error {
	_, err := kvutil.Update(ctx, s.groupsBucket, accountID+"."+groupName, iamRecordCASConfig("group", accountID, groupName), mutate)
	return err
}

// findGroupMembers scans the users bucket for any user in the account whose
// User.Groups references the given group. Fails closed on per-key Get or
// unmarshal errors so DeleteGroup cannot succeed while a real-but-unreadable
// member exists. Mirrors findInstanceProfilesForRole.
func (s *IAMServiceImpl) findGroupMembers(ctx context.Context, accountID, groupName string) ([]*User, error) {
	keys, err := kvutil.Keys(ctx, s.usersBucket)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("list user keys: %w", err)
	}

	keyPrefix := accountID + "."
	var members []*User
	for _, key := range keys {
		if key == kvutil.VersionKey {
			continue
		}
		if !strings.HasPrefix(key, keyPrefix) {
			continue
		}

		entry, err := s.usersBucket.Get(ctx, key)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				slog.Debug("findGroupMembers: user key disappeared", "key", key)
				continue
			}
			return nil, fmt.Errorf("get user %q: %w", key, err)
		}
		var user User
		if err := json.Unmarshal(entry.Value(), &user); err != nil {
			return nil, fmt.Errorf("unmarshal user %q: %w", key, err)
		}
		if slices.Contains(user.Groups, groupName) {
			u := user
			members = append(members, &u)
		}
	}
	return members, nil
}

// groupToSDK converts the internal Group record into the AWS SDK shape used by
// CreateGroup / GetGroup / ListGroups responses.
func groupToSDK(g *Group) *iam.Group {
	return &iam.Group{
		GroupName:  aws.String(g.GroupName),
		GroupId:    aws.String(g.GroupID),
		Arn:        aws.String(g.ARN),
		Path:       aws.String(g.Path),
		CreateDate: aws.Time(parseCreatedAt(g.CreatedAt)),
	}
}
