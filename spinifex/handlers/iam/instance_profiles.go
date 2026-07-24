package handlers_iam

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

func (s *IAMServiceImpl) CreateInstanceProfile(accountID string, input *iam.CreateInstanceProfileInput) (*iam.CreateInstanceProfileOutput, error) {
	ctx := context.Background()
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
		Tags:                copyTags(input.Tags),
	}

	data, err := json.Marshal(profile)
	if err != nil {
		return nil, fmt.Errorf("marshal instance profile: %w", err)
	}

	if _, err := s.instanceProfilesBucket.Create(ctx, accountID+"."+profileName, data); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return nil, errors.New(awserrors.ErrorIAMEntityAlreadyExists)
		}
		return nil, fmt.Errorf("store instance profile: %w", err)
	}

	slog.Info("IAM instance profile created",
		"accountID", accountID, "instanceProfileName", profileName, "instanceProfileID", profile.InstanceProfileID)

	sdkProfile, err := s.profileToSDK(ctx, accountID, &profile)
	if err != nil {
		return nil, err
	}
	return &iam.CreateInstanceProfileOutput{InstanceProfile: sdkProfile}, nil
}

func (s *IAMServiceImpl) GetInstanceProfile(accountID string, input *iam.GetInstanceProfileInput) (*iam.GetInstanceProfileOutput, error) {
	ctx := context.Background()
	profile, err := s.getInstanceProfile(ctx, accountID, *input.InstanceProfileName)
	if err != nil {
		return nil, err
	}

	sdkProfile, err := s.profileToSDK(ctx, accountID, profile)
	if err != nil {
		return nil, err
	}
	return &iam.GetInstanceProfileOutput{InstanceProfile: sdkProfile}, nil
}

func (s *IAMServiceImpl) ListInstanceProfiles(accountID string, input *iam.ListInstanceProfilesInput) (*iam.ListInstanceProfilesOutput, error) {
	ctx := context.Background()
	keys, err := kvutil.Keys(ctx, s.instanceProfilesBucket)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
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

		entry, err := s.instanceProfilesBucket.Get(ctx, key)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
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

		sdkProfile, err := s.profileToSDK(ctx, accountID, &profile)
		if err != nil {
			return nil, err
		}
		profiles = append(profiles, sdkProfile)
	}

	return &iam.ListInstanceProfilesOutput{
		InstanceProfiles: profiles,
		IsTruncated:      aws.Bool(false),
	}, nil
}

func (s *IAMServiceImpl) DeleteInstanceProfile(accountID string, input *iam.DeleteInstanceProfileInput) (*iam.DeleteInstanceProfileOutput, error) {
	ctx := context.Background()
	profileName := *input.InstanceProfileName

	profile, err := s.getInstanceProfile(ctx, accountID, profileName)
	if err != nil {
		return nil, err
	}

	if profile.RoleName != "" {
		return nil, errors.New(awserrors.ErrorIAMDeleteConflict)
	}

	if err := s.instanceProfilesBucket.Delete(ctx, accountID+"."+profileName); err != nil {
		return nil, fmt.Errorf("delete instance profile: %w", err)
	}

	slog.Info("IAM instance profile deleted", "accountID", accountID, "instanceProfileName", profileName)
	return &iam.DeleteInstanceProfileOutput{}, nil
}

func (s *IAMServiceImpl) AddRoleToInstanceProfile(accountID string, input *iam.AddRoleToInstanceProfileInput) (*iam.AddRoleToInstanceProfileOutput, error) {
	ctx := context.Background()
	profileName := *input.InstanceProfileName
	roleName := *input.RoleName

	if _, err := s.getRole(ctx, accountID, roleName); err != nil {
		return nil, err
	}

	profile, err := s.getInstanceProfile(ctx, accountID, profileName)
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
	if _, err := s.instanceProfilesBucket.Put(ctx, accountID+"."+profileName, data); err != nil {
		return nil, fmt.Errorf("update instance profile: %w", err)
	}

	slog.Info("IAM role added to instance profile",
		"accountID", accountID, "instanceProfileName", profileName, "roleName", roleName)
	return &iam.AddRoleToInstanceProfileOutput{}, nil
}

func (s *IAMServiceImpl) RemoveRoleFromInstanceProfile(accountID string, input *iam.RemoveRoleFromInstanceProfileInput) (*iam.RemoveRoleFromInstanceProfileOutput, error) {
	ctx := context.Background()
	profileName := *input.InstanceProfileName
	roleName := *input.RoleName

	profile, err := s.getInstanceProfile(ctx, accountID, profileName)
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
	if _, err := s.instanceProfilesBucket.Put(ctx, accountID+"."+profileName, data); err != nil {
		return nil, fmt.Errorf("update instance profile: %w", err)
	}

	slog.Info("IAM role removed from instance profile",
		"accountID", accountID, "instanceProfileName", profileName, "roleName", roleName)
	return &iam.RemoveRoleFromInstanceProfileOutput{}, nil
}

func (s *IAMServiceImpl) ListInstanceProfilesForRole(accountID string, input *iam.ListInstanceProfilesForRoleInput) (*iam.ListInstanceProfilesForRoleOutput, error) {
	ctx := context.Background()
	roleName := *input.RoleName

	if _, err := s.getRole(ctx, accountID, roleName); err != nil {
		return nil, err
	}

	profiles, err := s.findInstanceProfilesForRole(ctx, accountID, roleName)
	if err != nil {
		return nil, fmt.Errorf("find instance profiles for role: %w", err)
	}

	out := make([]*iam.InstanceProfile, 0, len(profiles))
	for _, p := range profiles {
		sdkProfile, err := s.profileToSDK(ctx, accountID, p)
		if err != nil {
			return nil, err
		}
		out = append(out, sdkProfile)
	}

	return &iam.ListInstanceProfilesForRoleOutput{
		InstanceProfiles: out,
		IsTruncated:      aws.Bool(false),
	}, nil
}

// ResolveInstanceProfile resolves an instance-profile name or ARN to its record.
// When given an ARN, the embedded account ID must match accountID.
func (s *IAMServiceImpl) ResolveInstanceProfile(accountID, nameOrARN string) (*InstanceProfile, error) {
	ctx := context.Background()
	if nameOrARN == "" {
		return nil, errors.New(awserrors.ErrorIAMInvalidInput)
	}

	if !strings.HasPrefix(nameOrARN, "arn:") {
		return s.getInstanceProfile(ctx, accountID, nameOrARN)
	}

	profileAccountID, profileName, err := parseInstanceProfileARN(nameOrARN)
	if err != nil {
		return nil, err
	}
	if profileAccountID != accountID {
		return nil, errors.New(awserrors.ErrorAccessDenied)
	}
	return s.getInstanceProfile(ctx, accountID, profileName)
}

// parseInstanceProfileARN extracts accountID and profile name from an IAM instance-profile ARN.
func parseInstanceProfileARN(arn string) (accountID, name string, err error) {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[1] != "aws" || parts[2] != "iam" || parts[3] != "" {
		return "", "", errors.New(awserrors.ErrorInvalidIamInstanceProfileArnMalformed)
	}
	resource := parts[5]
	const prefix = "instance-profile/"
	if !strings.HasPrefix(resource, prefix) {
		return "", "", errors.New(awserrors.ErrorInvalidIamInstanceProfileArnMalformed)
	}
	pathAndName := resource[len(prefix):]
	slash := strings.LastIndex(pathAndName, "/")
	if slash == -1 {
		name = pathAndName
	} else {
		name = pathAndName[slash+1:]
	}
	if name == "" {
		return "", "", errors.New(awserrors.ErrorInvalidIamInstanceProfileArnMalformed)
	}
	return parts[4], name, nil
}

// TagInstanceProfile upserts tags on an instance profile. Blind
// read-modify-write Put like the other instance-profile writers (no CAS).
func (s *IAMServiceImpl) TagInstanceProfile(accountID string, input *iam.TagInstanceProfileInput) (*iam.TagInstanceProfileOutput, error) {
	ctx := context.Background()
	if err := validateTags(input.Tags); err != nil {
		return nil, err
	}

	profileName := *input.InstanceProfileName
	profile, err := s.getInstanceProfile(ctx, accountID, profileName)
	if err != nil {
		return nil, err
	}

	merged := mergeTags(profile.Tags, input.Tags)
	if len(merged) > maxTagsPerResource {
		return nil, errors.New(awserrors.ErrorIAMLimitExceeded)
	}
	profile.Tags = merged

	data, err := json.Marshal(profile)
	if err != nil {
		return nil, fmt.Errorf("marshal instance profile: %w", err)
	}
	if _, err := s.instanceProfilesBucket.Put(ctx, accountID+"."+profileName, data); err != nil {
		return nil, fmt.Errorf("update instance profile: %w", err)
	}

	slog.Info("IAM instance profile tagged", "accountID", accountID, "instanceProfileName", profileName)
	return &iam.TagInstanceProfileOutput{}, nil
}

// UntagInstanceProfile removes the named tag keys from an instance profile;
// unknown keys are a no-op.
func (s *IAMServiceImpl) UntagInstanceProfile(accountID string, input *iam.UntagInstanceProfileInput) (*iam.UntagInstanceProfileOutput, error) {
	ctx := context.Background()
	profileName := *input.InstanceProfileName
	profile, err := s.getInstanceProfile(ctx, accountID, profileName)
	if err != nil {
		return nil, err
	}

	profile.Tags = removeTagKeys(profile.Tags, input.TagKeys)

	data, err := json.Marshal(profile)
	if err != nil {
		return nil, fmt.Errorf("marshal instance profile: %w", err)
	}
	if _, err := s.instanceProfilesBucket.Put(ctx, accountID+"."+profileName, data); err != nil {
		return nil, fmt.Errorf("update instance profile: %w", err)
	}

	slog.Info("IAM instance profile untagged", "accountID", accountID, "instanceProfileName", profileName)
	return &iam.UntagInstanceProfileOutput{}, nil
}

// ListInstanceProfileTags returns an instance profile's tags. Pagination is
// not implemented: IsTruncated is always false.
func (s *IAMServiceImpl) ListInstanceProfileTags(accountID string, input *iam.ListInstanceProfileTagsInput) (*iam.ListInstanceProfileTagsOutput, error) {
	ctx := context.Background()
	profile, err := s.getInstanceProfile(ctx, accountID, *input.InstanceProfileName)
	if err != nil {
		return nil, err
	}
	return &iam.ListInstanceProfileTagsOutput{
		Tags:        tagsToSDK(profile.Tags),
		IsTruncated: aws.Bool(false),
	}, nil
}

func (s *IAMServiceImpl) getInstanceProfile(ctx context.Context, accountID, profileName string) (*InstanceProfile, error) {
	entry, err := s.instanceProfilesBucket.Get(ctx, accountID+"."+profileName)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
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

// profileToSDK converts the internal InstanceProfile to the AWS SDK shape.
// Dereferences the attached role when present; propagates lookup errors.
func (s *IAMServiceImpl) profileToSDK(ctx context.Context, accountID string, p *InstanceProfile) (*iam.InstanceProfile, error) {
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
		role, err := s.getRole(ctx, accountID, p.RoleName)
		if err != nil {
			return nil, fmt.Errorf("resolve attached role %q on profile %q: %w", p.RoleName, p.InstanceProfileName, err)
		}
		out.Roles = append(out.Roles, roleToSDK(role))
	}
	return out, nil
}
