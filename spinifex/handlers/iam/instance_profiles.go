package handlers_iam

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// ---------------------------------------------------------------------------
// InstanceProfile CRUD
// ---------------------------------------------------------------------------

func (s *IAMServiceImpl) CreateInstanceProfile(accountID string, input *iam.CreateInstanceProfileInput) (*iam.CreateInstanceProfileOutput, error) {
	if input.InstanceProfileName == nil || *input.InstanceProfileName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	profileName := *input.InstanceProfileName

	if err := validatePolicyName(profileName); err != nil {
		return nil, errors.New(awserrors.ErrorIAMInvalidInput)
	}

	path := "/"
	if input.Path != nil {
		path = *input.Path
		if err := validatePath(path); err != nil {
			return nil, errors.New(awserrors.ErrorIAMInvalidInput)
		}
	}

	profileID, err := generateIAMID("AIPA")
	if err != nil {
		return nil, fmt.Errorf("generate instance profile ID: %w", err)
	}

	profile := InstanceProfile{
		InstanceProfileName: profileName,
		InstanceProfileID:   profileID,
		AccountID:           accountID,
		ARN:                 fmt.Sprintf("arn:aws:iam::%s:instance-profile%s%s", accountID, path, profileName),
		Path:                path,
		CreatedAt:           time.Now().UTC().Format(time.RFC3339),
		Tags:                []Tag{},
	}

	for _, tag := range input.Tags {
		if tag.Key != nil && tag.Value != nil {
			profile.Tags = append(profile.Tags, Tag{Key: *tag.Key, Value: *tag.Value})
		}
	}

	data, err := json.Marshal(profile)
	if err != nil {
		return nil, fmt.Errorf("marshal instance profile: %w", err)
	}

	if _, err := s.instanceProfilesBucket.Create(accountID+"."+profileName, data); err != nil {
		if errors.Is(err, nats.ErrKeyExists) {
			return nil, errors.New(awserrors.ErrorIAMEntityAlreadyExists)
		}
		return nil, fmt.Errorf("store instance profile: %w", err)
	}

	slog.Info("IAM instance profile created",
		"accountID", accountID, "instanceProfileName", profileName, "instanceProfileID", profile.InstanceProfileID)

	return &iam.CreateInstanceProfileOutput{InstanceProfile: s.profileToSDK(accountID, &profile)}, nil
}

func (s *IAMServiceImpl) GetInstanceProfile(accountID string, input *iam.GetInstanceProfileInput) (*iam.GetInstanceProfileOutput, error) {
	if input.InstanceProfileName == nil || *input.InstanceProfileName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	profile, err := s.getInstanceProfile(accountID, *input.InstanceProfileName)
	if err != nil {
		return nil, err
	}

	return &iam.GetInstanceProfileOutput{InstanceProfile: s.profileToSDK(accountID, profile)}, nil
}

func (s *IAMServiceImpl) ListInstanceProfiles(accountID string, input *iam.ListInstanceProfilesInput) (*iam.ListInstanceProfilesOutput, error) {
	keys, err := s.instanceProfilesBucket.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return &iam.ListInstanceProfilesOutput{
				InstanceProfiles: []*iam.InstanceProfile{},
				IsTruncated:      aws.Bool(false),
			}, nil
		}
		return nil, fmt.Errorf("list instance profile keys: %w", err)
	}

	pathPrefix := "/"
	if input.PathPrefix != nil {
		pathPrefix = *input.PathPrefix
	}

	keyPrefix := accountID + "."
	var profiles []*iam.InstanceProfile
	for _, key := range keys {
		if key == utils.VersionKey {
			continue
		}
		if !strings.HasPrefix(key, keyPrefix) {
			continue
		}

		entry, err := s.instanceProfilesBucket.Get(key)
		if err != nil {
			if errors.Is(err, nats.ErrKeyNotFound) {
				slog.Debug("ListInstanceProfiles: profile key disappeared (concurrent delete)", "key", key)
			} else {
				slog.Warn("ListInstanceProfiles: failed to get profile", "key", key, "err", err)
			}
			continue
		}

		var profile InstanceProfile
		if err := json.Unmarshal(entry.Value(), &profile); err != nil {
			slog.Warn("ListInstanceProfiles: failed to unmarshal profile", "key", key, "err", err)
			continue
		}

		if !strings.HasPrefix(profile.Path, pathPrefix) {
			continue
		}

		profiles = append(profiles, s.profileToSDK(accountID, &profile))
	}

	return &iam.ListInstanceProfilesOutput{
		InstanceProfiles: profiles,
		IsTruncated:      aws.Bool(false),
	}, nil
}

func (s *IAMServiceImpl) DeleteInstanceProfile(accountID string, input *iam.DeleteInstanceProfileInput) (*iam.DeleteInstanceProfileOutput, error) {
	if input.InstanceProfileName == nil || *input.InstanceProfileName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	profileName := *input.InstanceProfileName

	profile, err := s.getInstanceProfile(accountID, profileName)
	if err != nil {
		return nil, err
	}

	if profile.RoleName != "" {
		return nil, errors.New(awserrors.ErrorIAMDeleteConflict)
	}

	if err := s.instanceProfilesBucket.Delete(accountID + "." + profileName); err != nil {
		return nil, fmt.Errorf("delete instance profile: %w", err)
	}

	slog.Info("IAM instance profile deleted", "accountID", accountID, "instanceProfileName", profileName)
	return &iam.DeleteInstanceProfileOutput{}, nil
}

// ---------------------------------------------------------------------------
// InstanceProfile ↔ Role binding
// ---------------------------------------------------------------------------

func (s *IAMServiceImpl) AddRoleToInstanceProfile(accountID string, input *iam.AddRoleToInstanceProfileInput) (*iam.AddRoleToInstanceProfileOutput, error) {
	if input.InstanceProfileName == nil || *input.InstanceProfileName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.RoleName == nil || *input.RoleName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	profileName := *input.InstanceProfileName
	roleName := *input.RoleName

	if _, err := s.getRole(accountID, roleName); err != nil {
		return nil, err
	}

	profile, err := s.getInstanceProfile(accountID, profileName)
	if err != nil {
		return nil, err
	}

	if profile.RoleName != "" {
		return nil, errors.New(awserrors.ErrorIAMLimitExceeded)
	}

	profile.RoleName = roleName
	data, err := json.Marshal(profile)
	if err != nil {
		return nil, fmt.Errorf("marshal instance profile: %w", err)
	}
	if _, err := s.instanceProfilesBucket.Put(accountID+"."+profileName, data); err != nil {
		return nil, fmt.Errorf("update instance profile: %w", err)
	}

	slog.Info("IAM role added to instance profile",
		"accountID", accountID, "instanceProfileName", profileName, "roleName", roleName)
	return &iam.AddRoleToInstanceProfileOutput{}, nil
}

func (s *IAMServiceImpl) RemoveRoleFromInstanceProfile(accountID string, input *iam.RemoveRoleFromInstanceProfileInput) (*iam.RemoveRoleFromInstanceProfileOutput, error) {
	if input.InstanceProfileName == nil || *input.InstanceProfileName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.RoleName == nil || *input.RoleName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	profileName := *input.InstanceProfileName
	roleName := *input.RoleName

	profile, err := s.getInstanceProfile(accountID, profileName)
	if err != nil {
		return nil, err
	}

	if profile.RoleName == "" || profile.RoleName != roleName {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}

	profile.RoleName = ""
	data, err := json.Marshal(profile)
	if err != nil {
		return nil, fmt.Errorf("marshal instance profile: %w", err)
	}
	if _, err := s.instanceProfilesBucket.Put(accountID+"."+profileName, data); err != nil {
		return nil, fmt.Errorf("update instance profile: %w", err)
	}

	slog.Info("IAM role removed from instance profile",
		"accountID", accountID, "instanceProfileName", profileName, "roleName", roleName)
	return &iam.RemoveRoleFromInstanceProfileOutput{}, nil
}

func (s *IAMServiceImpl) ListInstanceProfilesForRole(accountID string, input *iam.ListInstanceProfilesForRoleInput) (*iam.ListInstanceProfilesForRoleOutput, error) {
	if input.RoleName == nil || *input.RoleName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	roleName := *input.RoleName

	if _, err := s.getRole(accountID, roleName); err != nil {
		return nil, err
	}

	profiles, err := s.findInstanceProfilesForRole(accountID, roleName)
	if err != nil {
		return nil, fmt.Errorf("find instance profiles for role: %w", err)
	}

	out := make([]*iam.InstanceProfile, 0, len(profiles))
	for _, p := range profiles {
		out = append(out, s.profileToSDK(accountID, p))
	}

	return &iam.ListInstanceProfilesForRoleOutput{
		InstanceProfiles: out,
		IsTruncated:      aws.Bool(false),
	}, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (s *IAMServiceImpl) getInstanceProfile(accountID, profileName string) (*InstanceProfile, error) {
	entry, err := s.instanceProfilesBucket.Get(accountID + "." + profileName)
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
		}
		return nil, fmt.Errorf("get instance profile: %w", err)
	}

	var profile InstanceProfile
	if err := json.Unmarshal(entry.Value(), &profile); err != nil {
		return nil, fmt.Errorf("unmarshal instance profile: %w", err)
	}
	return &profile, nil
}

// profileToSDK converts the internal InstanceProfile record into the AWS SDK
// shape. The SDK requires a Roles array (0 or 1 element in our model); when
// a role is attached the role's full record is dereferenced and embedded.
// A missing role record is treated as no-role rather than failing the call —
// the caller is typically already aware of the inconsistency from a separate
// DeleteRole / RemoveRole codepath.
func (s *IAMServiceImpl) profileToSDK(accountID string, p *InstanceProfile) *iam.InstanceProfile {
	out := &iam.InstanceProfile{
		InstanceProfileName: aws.String(p.InstanceProfileName),
		InstanceProfileId:   aws.String(p.InstanceProfileID),
		Arn:                 aws.String(p.ARN),
		Path:                aws.String(p.Path),
		CreateDate:          aws.Time(parseCreatedAt(p.CreatedAt)),
		Roles:               []*iam.Role{},
	}
	for _, t := range p.Tags {
		out.Tags = append(out.Tags, &iam.Tag{
			Key:   aws.String(t.Key),
			Value: aws.String(t.Value),
		})
	}
	if p.RoleName != "" {
		role, err := s.getRole(accountID, p.RoleName)
		if err != nil {
			slog.Warn("profileToSDK: attached role not found",
				"accountID", accountID, "instanceProfileName", p.InstanceProfileName, "roleName", p.RoleName, "err", err)
			return out
		}
		out.Roles = append(out.Roles, roleToSDK(role))
	}
	return out
}
