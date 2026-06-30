package handlers_iam

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// maxGroupsPerUser caps how many groups a single user may belong to. Mirrors the
// hardcoded maxAccessKeysPerUser limit and bounds the per-request getGroup
// fan-out that GetUserPolicies performs on the authorization hot path.
const maxGroupsPerUser = 10

func (s *IAMServiceImpl) CreateGroup(accountID string, input *iam.CreateGroupInput) (*iam.CreateGroupOutput, error) {
	groupName := *input.GroupName
	if err := validateGroupName(groupName); err != nil {
		return nil, errors.New(awserrors.ErrorIAMInvalidInput)
	}

	path := "/"
	if input.Path != nil {
		path = *input.Path
		if err := validatePath(path); err != nil {
			return nil, errors.New(awserrors.ErrorIAMInvalidInput)
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
		ARN:              fmt.Sprintf("arn:aws:iam::%s:group%s%s", accountID, path, groupName),
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
	if _, err := s.groupsBucket.Create(kvKey, data); err != nil {
		if errors.Is(err, nats.ErrKeyExists) {
			return nil, errors.New(awserrors.ErrorIAMEntityAlreadyExists)
		}
		return nil, fmt.Errorf("store group: %w", err)
	}

	slog.Info("IAM group created", "accountID", accountID, "groupName", groupName, "groupID", group.GroupID)

	return &iam.CreateGroupOutput{Group: groupToSDK(&group)}, nil
}

func (s *IAMServiceImpl) GetGroup(accountID string, input *iam.GetGroupInput) (*iam.GetGroupOutput, error) {
	group, err := s.getGroup(accountID, *input.GroupName)
	if err != nil {
		return nil, err
	}

	members, err := s.findGroupMembers(accountID, group.GroupName)
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
	keys, err := s.groupsBucket.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
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
	var groups []*iam.Group
	for _, key := range keys {
		if key == utils.VersionKey {
			continue
		}
		if !strings.HasPrefix(key, keyPrefix) {
			continue
		}

		entry, err := s.groupsBucket.Get(key)
		if err != nil {
			if errors.Is(err, nats.ErrKeyNotFound) {
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
	groupName := *input.GroupName

	group, err := s.getGroup(accountID, groupName)
	if err != nil {
		return nil, err
	}

	if len(group.AttachedPolicies) > 0 {
		return nil, errors.New(awserrors.ErrorIAMDeleteConflict)
	}

	members, err := s.findGroupMembers(accountID, groupName)
	if err != nil {
		return nil, fmt.Errorf("check group members: %w", err)
	}
	if len(members) > 0 {
		return nil, errors.New(awserrors.ErrorIAMDeleteConflict)
	}

	if err := s.groupsBucket.Delete(accountID + "." + groupName); err != nil {
		return nil, fmt.Errorf("delete group: %w", err)
	}

	slog.Info("IAM group deleted", "accountID", accountID, "groupName", groupName)
	return &iam.DeleteGroupOutput{}, nil
}

// ---------------------------------------------------------------------------
// Group membership — canonical on the User record (User.Groups)
// ---------------------------------------------------------------------------

func (s *IAMServiceImpl) AddUserToGroup(accountID string, input *iam.AddUserToGroupInput) (*iam.AddUserToGroupOutput, error) {
	groupName := *input.GroupName
	userName := *input.UserName
	userKVKey := accountID + "." + userName

	if _, err := s.getGroup(accountID, groupName); err != nil {
		return nil, err
	}

	user, err := s.getUser(accountID, userName)
	if err != nil {
		return nil, err
	}

	if slices.Contains(user.Groups, groupName) { // idempotent
		return &iam.AddUserToGroupOutput{}, nil
	}
	if len(user.Groups) >= maxGroupsPerUser {
		return nil, errors.New(awserrors.ErrorIAMLimitExceeded)
	}

	user.Groups = append(user.Groups, groupName)
	userData, err := json.Marshal(user)
	if err != nil {
		return nil, fmt.Errorf("marshal user: %w", err)
	}

	if _, err := s.usersBucket.Put(userKVKey, userData); err != nil {
		return nil, fmt.Errorf("update user: %w", err)
	}

	slog.Info("IAM user added to group", "accountID", accountID, "userName", userName, "groupName", groupName)
	return &iam.AddUserToGroupOutput{}, nil
}

func (s *IAMServiceImpl) RemoveUserFromGroup(accountID string, input *iam.RemoveUserFromGroupInput) (*iam.RemoveUserFromGroupOutput, error) {
	groupName := *input.GroupName
	userName := *input.UserName
	userKVKey := accountID + "." + userName

	user, err := s.getUser(accountID, userName)
	if err != nil {
		return nil, err
	}

	// Operate purely on the membership reference so a dangling pointer to an
	// already-deleted group is still cleanable; never fetch the group here.
	found := false
	remaining := make([]string, 0, len(user.Groups))
	for _, name := range user.Groups {
		if name == groupName {
			found = true
		} else {
			remaining = append(remaining, name)
		}
	}

	if !found {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}

	user.Groups = remaining
	userData, err := json.Marshal(user)
	if err != nil {
		return nil, fmt.Errorf("marshal user: %w", err)
	}

	if _, err := s.usersBucket.Put(userKVKey, userData); err != nil {
		return nil, fmt.Errorf("update user: %w", err)
	}

	slog.Info("IAM user removed from group", "accountID", accountID, "userName", userName, "groupName", groupName)
	return &iam.RemoveUserFromGroupOutput{}, nil
}

func (s *IAMServiceImpl) ListGroupsForUser(accountID string, input *iam.ListGroupsForUserInput) (*iam.ListGroupsForUserOutput, error) {
	user, err := s.getUser(accountID, *input.UserName)
	if err != nil {
		return nil, err
	}

	var groups []*iam.Group
	for _, name := range user.Groups {
		group, err := s.getGroup(accountID, name)
		if err != nil {
			// Skip a membership whose group has vanished, consistent with
			// GetUserPolicies; a dangling pointer is inert, not an error.
			slog.Warn("ListGroupsForUser: member references missing group; skipping",
				"accountID", accountID, "user", *input.UserName, "group", name)
			continue
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
	groupName := *input.GroupName
	policyARN := *input.PolicyArn
	kvKey := accountID + "." + groupName

	// AWS-managed ARNs are stored opaquely (like roles); customer-managed ARNs must exist.
	if !isAWSManagedPolicyARN(policyARN) {
		if _, err := s.getPolicyByARN(accountID, policyARN); err != nil {
			return nil, err
		}
	}

	group, err := s.getGroup(accountID, groupName)
	if err != nil {
		return nil, err
	}

	if slices.Contains(group.AttachedPolicies, policyARN) { // idempotent
		return &iam.AttachGroupPolicyOutput{}, nil
	}

	group.AttachedPolicies = append(group.AttachedPolicies, policyARN)
	data, err := json.Marshal(group)
	if err != nil {
		return nil, fmt.Errorf("marshal group: %w", err)
	}

	if _, err := s.groupsBucket.Put(kvKey, data); err != nil {
		return nil, fmt.Errorf("update group: %w", err)
	}

	slog.Info("IAM policy attached to group", "accountID", accountID, "groupName", groupName, "policyArn", policyARN)
	return &iam.AttachGroupPolicyOutput{}, nil
}

func (s *IAMServiceImpl) DetachGroupPolicy(accountID string, input *iam.DetachGroupPolicyInput) (*iam.DetachGroupPolicyOutput, error) {
	groupName := *input.GroupName
	policyARN := *input.PolicyArn
	kvKey := accountID + "." + groupName

	group, err := s.getGroup(accountID, groupName)
	if err != nil {
		return nil, err
	}

	idx := slices.Index(group.AttachedPolicies, policyARN)
	if idx < 0 {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	group.AttachedPolicies = slices.Delete(group.AttachedPolicies, idx, idx+1)

	data, err := json.Marshal(group)
	if err != nil {
		return nil, fmt.Errorf("marshal group: %w", err)
	}

	if _, err := s.groupsBucket.Put(kvKey, data); err != nil {
		return nil, fmt.Errorf("update group: %w", err)
	}

	slog.Info("IAM policy detached from group", "accountID", accountID, "groupName", groupName, "policyArn", policyARN)
	return &iam.DetachGroupPolicyOutput{}, nil
}

func (s *IAMServiceImpl) ListAttachedGroupPolicies(accountID string, input *iam.ListAttachedGroupPoliciesInput) (*iam.ListAttachedGroupPoliciesOutput, error) {
	group, err := s.getGroup(accountID, *input.GroupName)
	if err != nil {
		return nil, err
	}

	var attached []*iam.AttachedPolicy
	for _, arn := range group.AttachedPolicies {
		if isAWSManagedPolicyARN(arn) {
			attached = append(attached, &iam.AttachedPolicy{
				PolicyArn:  aws.String(arn),
				PolicyName: aws.String(managedPolicyNameFromARN(arn)),
			})
			continue
		}
		policy, err := s.getPolicyByARN(accountID, arn)
		if err != nil {
			slog.Warn("ListAttachedGroupPolicies: policy not found for ARN", "arn", arn, "err", err)
			continue
		}
		attached = append(attached, &iam.AttachedPolicy{
			PolicyArn:  aws.String(policy.ARN),
			PolicyName: aws.String(policy.PolicyName),
		})
	}

	return &iam.ListAttachedGroupPoliciesOutput{
		AttachedPolicies: attached,
		IsTruncated:      aws.Bool(false),
	}, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (s *IAMServiceImpl) getGroup(accountID, groupName string) (*Group, error) {
	entry, err := s.groupsBucket.Get(accountID + "." + groupName)
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
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

// findGroupMembers scans the users bucket for any user in the account whose
// User.Groups references the given group. Fails closed on per-key Get or
// unmarshal errors so DeleteGroup cannot succeed while a real-but-unreadable
// member exists. Mirrors findInstanceProfilesForRole.
func (s *IAMServiceImpl) findGroupMembers(accountID, groupName string) ([]*User, error) {
	keys, err := s.usersBucket.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("list user keys: %w", err)
	}

	keyPrefix := accountID + "."
	var members []*User
	for _, key := range keys {
		if key == utils.VersionKey {
			continue
		}
		if !strings.HasPrefix(key, keyPrefix) {
			continue
		}

		entry, err := s.usersBucket.Get(key)
		if err != nil {
			if errors.Is(err, nats.ErrKeyNotFound) {
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

// validateGroupName enforces the IAM group-name limits: 1–128 chars from the
// IAM name charset. The constraints match validatePolicyName exactly.
func validateGroupName(name string) error {
	return validatePolicyName(name)
}
