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

const (
	// AWS limits trust policies to 2048 bytes — distinct from the 6144-byte
	// limit applied to managed policy documents.
	maxTrustPolicyDocumentSize = 2048

	defaultMaxSessionDuration = int64(3600)
	minMaxSessionDuration     = int64(900)
	maxMaxSessionDuration     = int64(43200)
)

func (s *IAMServiceImpl) CreateRole(accountID string, input *iam.CreateRoleInput) (*iam.CreateRoleOutput, error) {
	roleName := *input.RoleName
	if err := validateUserName(roleName); err != nil {
		return nil, errors.New(awserrors.ErrorIAMInvalidInput)
	}

	path := "/"
	if input.Path != nil {
		path = *input.Path
		if err := validatePath(path); err != nil {
			return nil, errors.New(awserrors.ErrorIAMInvalidInput)
		}
	}

	if _, err := ValidateTrustPolicyDocument(*input.AssumeRolePolicyDocument); err != nil {
		slog.Debug("CreateRole: invalid trust policy", "roleName", roleName, "err", err)
		return nil, errors.New(awserrors.ErrorIAMMalformedPolicyDocument)
	}

	maxSession := defaultMaxSessionDuration
	if input.MaxSessionDuration != nil {
		maxSession = *input.MaxSessionDuration
		if maxSession < minMaxSessionDuration || maxSession > maxMaxSessionDuration {
			return nil, errors.New(awserrors.ErrorValidationError)
		}
	}

	roleID, err := generateIAMID("AROA")
	if err != nil {
		return nil, fmt.Errorf("generate role ID: %w", err)
	}

	role := Role{
		RoleName:                 roleName,
		RoleID:                   roleID,
		AccountID:                accountID,
		ARN:                      fmt.Sprintf("arn:aws:iam::%s:role%s%s", accountID, path, roleName),
		Path:                     path,
		Description:              aws.StringValue(input.Description),
		AssumeRolePolicyDocument: *input.AssumeRolePolicyDocument,
		MaxSessionDuration:       maxSession,
		CreatedAt:                time.Now().UTC().Format(time.RFC3339),
		AttachedPolicies:         []string{},
		Tags:                     []Tag{},
	}

	for _, tag := range input.Tags {
		if tag.Key != nil && tag.Value != nil {
			role.Tags = append(role.Tags, Tag{Key: *tag.Key, Value: *tag.Value})
		}
	}

	data, err := json.Marshal(role)
	if err != nil {
		return nil, fmt.Errorf("marshal role: %w", err)
	}

	kvKey := accountID + "." + roleName
	if _, err := s.rolesBucket.Create(kvKey, data); err != nil {
		if errors.Is(err, nats.ErrKeyExists) {
			return nil, errors.New(awserrors.ErrorIAMEntityAlreadyExists)
		}
		return nil, fmt.Errorf("store role: %w", err)
	}

	slog.Info("IAM role created", "accountID", accountID, "roleName", roleName, "roleID", role.RoleID)

	return &iam.CreateRoleOutput{Role: roleToSDK(&role)}, nil
}

func (s *IAMServiceImpl) GetRole(accountID string, input *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
	role, err := s.getRole(accountID, *input.RoleName)
	if err != nil {
		return nil, err
	}

	return &iam.GetRoleOutput{Role: roleToSDK(role)}, nil
}

func (s *IAMServiceImpl) ListRoles(accountID string, input *iam.ListRolesInput) (*iam.ListRolesOutput, error) {
	keys, err := s.rolesBucket.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return &iam.ListRolesOutput{
				Roles:       []*iam.Role{},
				IsTruncated: aws.Bool(false),
			}, nil
		}
		return nil, fmt.Errorf("list role keys: %w", err)
	}

	pathPrefix := "/"
	if input.PathPrefix != nil {
		pathPrefix = *input.PathPrefix
	}

	keyPrefix := accountID + "."
	var roles []*iam.Role
	for _, key := range keys {
		if key == utils.VersionKey {
			continue
		}
		if !strings.HasPrefix(key, keyPrefix) {
			continue
		}

		entry, err := s.rolesBucket.Get(key)
		if err != nil {
			if errors.Is(err, nats.ErrKeyNotFound) {
				slog.Debug("ListRoles: role key disappeared (concurrent delete)", "key", key)
			} else {
				slog.Warn("ListRoles: failed to get role", "key", key, "err", err)
			}
			continue
		}

		var role Role
		if err := json.Unmarshal(entry.Value(), &role); err != nil {
			slog.Warn("ListRoles: failed to unmarshal role", "key", key, "err", err)
			continue
		}

		if !strings.HasPrefix(role.Path, pathPrefix) {
			continue
		}

		roles = append(roles, roleToSDK(&role))
	}

	return &iam.ListRolesOutput{
		Roles:       roles,
		IsTruncated: aws.Bool(false),
	}, nil
}

func (s *IAMServiceImpl) DeleteRole(accountID string, input *iam.DeleteRoleInput) (*iam.DeleteRoleOutput, error) {
	roleName := *input.RoleName

	role, err := s.getRole(accountID, roleName)
	if err != nil {
		return nil, err
	}

	if len(role.AttachedPolicies) > 0 {
		return nil, errors.New(awserrors.ErrorIAMDeleteConflict)
	}

	profiles, err := s.findInstanceProfilesForRole(accountID, roleName)
	if err != nil {
		return nil, fmt.Errorf("check role instance profiles: %w", err)
	}
	if len(profiles) > 0 {
		return nil, errors.New(awserrors.ErrorIAMDeleteConflict)
	}

	if err := s.rolesBucket.Delete(accountID + "." + roleName); err != nil {
		return nil, fmt.Errorf("delete role: %w", err)
	}

	slog.Info("IAM role deleted", "accountID", accountID, "roleName", roleName)
	return &iam.DeleteRoleOutput{}, nil
}

func (s *IAMServiceImpl) UpdateRole(accountID string, input *iam.UpdateRoleInput) (*iam.UpdateRoleOutput, error) {
	roleName := *input.RoleName

	role, err := s.getRole(accountID, roleName)
	if err != nil {
		return nil, err
	}

	if input.Description != nil {
		role.Description = *input.Description
	}
	if input.MaxSessionDuration != nil {
		dur := *input.MaxSessionDuration
		if dur < minMaxSessionDuration || dur > maxMaxSessionDuration {
			return nil, errors.New(awserrors.ErrorValidationError)
		}
		role.MaxSessionDuration = dur
	}

	data, err := json.Marshal(role)
	if err != nil {
		return nil, fmt.Errorf("marshal role: %w", err)
	}
	if _, err := s.rolesBucket.Put(accountID+"."+roleName, data); err != nil {
		return nil, fmt.Errorf("update role: %w", err)
	}

	slog.Info("IAM role updated", "accountID", accountID, "roleName", roleName)
	return &iam.UpdateRoleOutput{}, nil
}

func (s *IAMServiceImpl) UpdateAssumeRolePolicy(accountID string, input *iam.UpdateAssumeRolePolicyInput) (*iam.UpdateAssumeRolePolicyOutput, error) {
	roleName := *input.RoleName

	if _, err := ValidateTrustPolicyDocument(*input.PolicyDocument); err != nil {
		slog.Debug("UpdateAssumeRolePolicy: invalid trust policy", "roleName", roleName, "err", err)
		return nil, errors.New(awserrors.ErrorIAMMalformedPolicyDocument)
	}

	role, err := s.getRole(accountID, roleName)
	if err != nil {
		return nil, err
	}

	role.AssumeRolePolicyDocument = *input.PolicyDocument

	data, err := json.Marshal(role)
	if err != nil {
		return nil, fmt.Errorf("marshal role: %w", err)
	}
	if _, err := s.rolesBucket.Put(accountID+"."+roleName, data); err != nil {
		return nil, fmt.Errorf("update role trust policy: %w", err)
	}

	slog.Info("IAM role trust policy updated", "accountID", accountID, "roleName", roleName)
	return &iam.UpdateAssumeRolePolicyOutput{}, nil
}

func (s *IAMServiceImpl) AttachRolePolicy(accountID string, input *iam.AttachRolePolicyInput) (*iam.AttachRolePolicyOutput, error) {
	roleName := *input.RoleName
	policyARN := *input.PolicyArn

	if _, err := s.getPolicyByARN(accountID, policyARN); err != nil {
		return nil, err
	}

	role, err := s.getRole(accountID, roleName)
	if err != nil {
		return nil, err
	}

	if slices.Contains(role.AttachedPolicies, policyARN) {
		return &iam.AttachRolePolicyOutput{}, nil
	}

	role.AttachedPolicies = append(role.AttachedPolicies, policyARN)
	data, err := json.Marshal(role)
	if err != nil {
		return nil, fmt.Errorf("marshal role: %w", err)
	}
	if _, err := s.rolesBucket.Put(accountID+"."+roleName, data); err != nil {
		return nil, fmt.Errorf("update role: %w", err)
	}

	slog.Info("IAM policy attached to role", "accountID", accountID, "roleName", roleName, "policyArn", policyARN)
	return &iam.AttachRolePolicyOutput{}, nil
}

func (s *IAMServiceImpl) DetachRolePolicy(accountID string, input *iam.DetachRolePolicyInput) (*iam.DetachRolePolicyOutput, error) {
	roleName := *input.RoleName
	policyARN := *input.PolicyArn

	role, err := s.getRole(accountID, roleName)
	if err != nil {
		return nil, err
	}

	found := false
	remaining := make([]string, 0, len(role.AttachedPolicies))
	for _, arn := range role.AttachedPolicies {
		if arn == policyARN {
			found = true
		} else {
			remaining = append(remaining, arn)
		}
	}
	if !found {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}

	role.AttachedPolicies = remaining
	data, err := json.Marshal(role)
	if err != nil {
		return nil, fmt.Errorf("marshal role: %w", err)
	}
	if _, err := s.rolesBucket.Put(accountID+"."+roleName, data); err != nil {
		return nil, fmt.Errorf("update role: %w", err)
	}

	slog.Info("IAM policy detached from role", "accountID", accountID, "roleName", roleName, "policyArn", policyARN)
	return &iam.DetachRolePolicyOutput{}, nil
}

func (s *IAMServiceImpl) ListAttachedRolePolicies(accountID string, input *iam.ListAttachedRolePoliciesInput) (*iam.ListAttachedRolePoliciesOutput, error) {
	role, err := s.getRole(accountID, *input.RoleName)
	if err != nil {
		return nil, err
	}

	var attached []*iam.AttachedPolicy
	for _, arn := range role.AttachedPolicies {
		policy, err := s.getPolicyByARN(accountID, arn)
		if err != nil {
			slog.Warn("ListAttachedRolePolicies: policy not found for ARN", "arn", arn, "err", err)
			continue
		}
		attached = append(attached, &iam.AttachedPolicy{
			PolicyArn:  aws.String(policy.ARN),
			PolicyName: aws.String(policy.PolicyName),
		})
	}

	return &iam.ListAttachedRolePoliciesOutput{
		AttachedPolicies: attached,
		IsTruncated:      aws.Bool(false),
	}, nil
}

func (s *IAMServiceImpl) getRole(accountID, roleName string) (*Role, error) {
	entry, err := s.rolesBucket.Get(accountID + "." + roleName)
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
		}
		return nil, fmt.Errorf("get role: %w", err)
	}

	var role Role
	if err := json.Unmarshal(entry.Value(), &role); err != nil {
		return nil, fmt.Errorf("unmarshal role: %w", err)
	}
	return &role, nil
}

// findInstanceProfilesForRole scans the instance-profiles bucket for any
// profile in the account that references the given role. Fails closed on
// per-key Get or unmarshal errors so DeleteRole cannot succeed while a
// real-but-unreadable reference exists.
func (s *IAMServiceImpl) findInstanceProfilesForRole(accountID, roleName string) ([]*InstanceProfile, error) {
	keys, err := s.instanceProfilesBucket.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("list instance profile keys: %w", err)
	}

	keyPrefix := accountID + "."
	var profiles []*InstanceProfile
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
				slog.Debug("findInstanceProfilesForRole: profile key disappeared", "key", key)
				continue
			}
			return nil, fmt.Errorf("get instance profile %q: %w", key, err)
		}
		var profile InstanceProfile
		if err := json.Unmarshal(entry.Value(), &profile); err != nil {
			return nil, fmt.Errorf("unmarshal instance profile %q: %w", key, err)
		}
		if profile.RoleName == roleName {
			p := profile
			profiles = append(profiles, &p)
		}
	}
	return profiles, nil
}

// roleToSDK converts the internal Role record into the AWS SDK shape used by
// CreateRole / GetRole / ListRoles responses.
func roleToSDK(r *Role) *iam.Role {
	out := &iam.Role{
		RoleName:                 aws.String(r.RoleName),
		RoleId:                   aws.String(r.RoleID),
		Arn:                      aws.String(r.ARN),
		Path:                     aws.String(r.Path),
		AssumeRolePolicyDocument: aws.String(r.AssumeRolePolicyDocument),
		CreateDate:               aws.Time(parseCreatedAt(r.CreatedAt)),
		MaxSessionDuration:       aws.Int64(r.MaxSessionDuration),
	}
	if r.Description != "" {
		out.Description = aws.String(r.Description)
	}
	for _, t := range r.Tags {
		out.Tags = append(out.Tags, &iam.Tag{
			Key:   aws.String(t.Key),
			Value: aws.String(t.Value),
		})
	}
	return out
}

// ValidateTrustPolicyDocument parses and validates an AssumeRolePolicyDocument
// JSON string. Trust-policy evaluation (Principal semantics, Action being
// sts:AssumeRole, etc.) is deferred to STS — this layer only checks the
// document shape.
//
// Three categories of fields are rejected at write time rather than silently
// accepted-then-ignored at evaluation time: Condition blocks, NotPrincipal,
// and NotAction. Each would otherwise let an author write a policy whose
// runtime behaviour silently diverges from its stated intent (e.g. an
// ExternalId-protected role that ignores ExternalId, or a NotPrincipal-Allow
// that grants the universe). Loud failure here beats silent allow there.
func ValidateTrustPolicyDocument(docJSON string) (*TrustPolicyDocument, error) {
	if len(docJSON) > maxTrustPolicyDocumentSize {
		return nil, fmt.Errorf("trust policy exceeds maximum size of %d bytes", maxTrustPolicyDocumentSize)
	}

	var doc TrustPolicyDocument
	if err := json.Unmarshal([]byte(docJSON), &doc); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}

	if doc.Version != "2012-10-17" {
		return nil, fmt.Errorf("unsupported trust policy version: %q", doc.Version)
	}

	if len(doc.Statement) == 0 {
		return nil, fmt.Errorf("trust policy must contain at least one statement")
	}

	for i, stmt := range doc.Statement {
		if stmt.Effect != PolicyEffectAllow && stmt.Effect != PolicyEffectDeny {
			return nil, fmt.Errorf("statement %d: Effect must be Allow or Deny, got %q", i, stmt.Effect)
		}
		if !isRawJSONNonEmpty(stmt.Principal) {
			return nil, fmt.Errorf("statement %d: Principal is required", i)
		}
		if len(stmt.Action) == 0 {
			return nil, fmt.Errorf("statement %d: Action is required", i)
		}
		for j, a := range stmt.Action {
			if a == "" {
				return nil, fmt.Errorf("statement %d: Action element %d must not be empty", i, j)
			}
		}
		if isRawJSONNonEmpty(stmt.Condition) {
			return nil, fmt.Errorf("statement %d: trust policy Condition blocks are not supported in this release; remove the Condition field or wait for v1.1", i)
		}
		if isRawJSONNonEmpty(stmt.NotPrincipal) {
			return nil, fmt.Errorf("statement %d: trust policy NotPrincipal blocks are not supported in this release; use Principal with an explicit allow-list instead", i)
		}
		if len(stmt.NotAction) > 0 {
			return nil, fmt.Errorf("statement %d: trust policy NotAction blocks are not supported in this release", i)
		}
	}

	return &doc, nil
}

// isRawJSONNonEmpty reports whether a json.RawMessage carries a meaningful
// value. Whitespace, the JSON null literal, and the empty object/array
// literals all count as empty — they would otherwise let `Condition: {}`
// trip the Condition rejection and let `Principal: {}` slip past the
// Principal-is-required check.
func isRawJSONNonEmpty(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	switch strings.TrimSpace(string(raw)) {
	case "", "null", "{}", "[]":
		return false
	default:
		return true
	}
}
