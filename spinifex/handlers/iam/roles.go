package handlers_iam

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

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

const (
	// AWS limits trust policies to 2048 bytes — distinct from the 6144-byte
	// limit applied to managed policy documents.
	maxTrustPolicyDocumentSize = 2048

	defaultMaxSessionDuration = int64(3600)
	minMaxSessionDuration     = int64(900)
	maxMaxSessionDuration     = int64(43200)

	// Bound on optimistic-concurrency retries when a concurrent writer wins the
	// CAS race on a role record. High enough to absorb the handful of parallel
	// AttachRolePolicy calls Terraform fans out per role.
	roleCASMaxRetries = 16
)

func (s *IAMServiceImpl) CreateRole(accountID string, input *iam.CreateRoleInput) (*iam.CreateRoleOutput, error) {
	ctx := context.Background()
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
		Tags:                     copyTags(input.Tags),
	}

	data, err := json.Marshal(role)
	if err != nil {
		return nil, fmt.Errorf("marshal role: %w", err)
	}

	kvKey := accountID + "." + roleName
	if _, err := s.rolesBucket.Create(ctx, kvKey, data); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return nil, errors.New(awserrors.ErrorIAMEntityAlreadyExists)
		}
		return nil, fmt.Errorf("store role: %w", err)
	}

	slog.Info("IAM role created", "accountID", accountID, "roleName", roleName, "roleID", role.RoleID)

	return &iam.CreateRoleOutput{Role: roleToSDK(&role)}, nil
}

func (s *IAMServiceImpl) GetRole(accountID string, input *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
	ctx := context.Background()
	role, err := s.getRole(ctx, accountID, *input.RoleName)
	if err != nil {
		return nil, err
	}

	return &iam.GetRoleOutput{Role: roleToSDK(role)}, nil
}

func (s *IAMServiceImpl) ListRoles(accountID string, input *iam.ListRolesInput) (*iam.ListRolesOutput, error) {
	ctx := context.Background()
	keys, err := kvutil.Keys(ctx, s.rolesBucket)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
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

		entry, err := s.rolesBucket.Get(ctx, key)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
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
	ctx := context.Background()
	roleName := *input.RoleName

	role, err := s.getRole(ctx, accountID, roleName)
	if err != nil {
		return nil, err
	}

	if len(role.AttachedPolicies) > 0 {
		return nil, errors.New(awserrors.ErrorIAMDeleteConflict)
	}

	if len(role.InlinePolicies) > 0 {
		return nil, errors.New(awserrors.ErrorIAMDeleteConflict)
	}

	profiles, err := s.findInstanceProfilesForRole(ctx, accountID, roleName)
	if err != nil {
		return nil, fmt.Errorf("check role instance profiles: %w", err)
	}
	if len(profiles) > 0 {
		return nil, errors.New(awserrors.ErrorIAMDeleteConflict)
	}

	if err := s.rolesBucket.Delete(ctx, accountID+"."+roleName); err != nil {
		return nil, fmt.Errorf("delete role: %w", err)
	}

	slog.Info("IAM role deleted", "accountID", accountID, "roleName", roleName)
	return &iam.DeleteRoleOutput{}, nil
}

func (s *IAMServiceImpl) UpdateRole(accountID string, input *iam.UpdateRoleInput) (*iam.UpdateRoleOutput, error) {
	ctx := context.Background()
	roleName := *input.RoleName

	role, err := s.getRole(ctx, accountID, roleName)
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
	if _, err := s.rolesBucket.Put(ctx, accountID+"."+roleName, data); err != nil {
		return nil, fmt.Errorf("update role: %w", err)
	}

	slog.Info("IAM role updated", "accountID", accountID, "roleName", roleName)
	return &iam.UpdateRoleOutput{}, nil
}

func (s *IAMServiceImpl) UpdateAssumeRolePolicy(accountID string, input *iam.UpdateAssumeRolePolicyInput) (*iam.UpdateAssumeRolePolicyOutput, error) {
	ctx := context.Background()
	roleName := *input.RoleName

	if _, err := ValidateTrustPolicyDocument(*input.PolicyDocument); err != nil {
		slog.Debug("UpdateAssumeRolePolicy: invalid trust policy", "roleName", roleName, "err", err)
		return nil, errors.New(awserrors.ErrorIAMMalformedPolicyDocument)
	}

	role, err := s.getRole(ctx, accountID, roleName)
	if err != nil {
		return nil, err
	}

	role.AssumeRolePolicyDocument = *input.PolicyDocument

	data, err := json.Marshal(role)
	if err != nil {
		return nil, fmt.Errorf("marshal role: %w", err)
	}
	if _, err := s.rolesBucket.Put(ctx, accountID+"."+roleName, data); err != nil {
		return nil, fmt.Errorf("update role trust policy: %w", err)
	}

	slog.Info("IAM role trust policy updated", "accountID", accountID, "roleName", roleName)
	return &iam.UpdateAssumeRolePolicyOutput{}, nil
}

func (s *IAMServiceImpl) AttachRolePolicy(accountID string, input *iam.AttachRolePolicyInput) (*iam.AttachRolePolicyOutput, error) {
	ctx := context.Background()
	roleName := *input.RoleName
	policyARN := *input.PolicyArn

	// AWS-managed policy ARNs (arn:aws:iam::aws:policy/...) are never provisioned
	// in Spinifex; stock EKS tooling attaches them (AmazonEKSClusterPolicy,
	// AmazonEKSWorkerNodePolicy, AmazonEKS_CNI_Policy, ...). Store them opaquely
	// so ListAttachedRolePolicies / DescribeNodegroup round-trip instead of
	// failing NoSuchEntity. Customer-managed ARNs must still exist.
	if !isAWSManagedPolicyARN(policyARN) {
		if _, err := s.getPolicyByARN(ctx, accountID, policyARN); err != nil {
			return nil, err
		}
	}

	err := s.updateRoleCAS(ctx, accountID, roleName, func(role *Role) (bool, error) {
		if slices.Contains(role.AttachedPolicies, policyARN) {
			return false, nil
		}
		role.AttachedPolicies = append(role.AttachedPolicies, policyARN)
		return true, nil
	})
	if err != nil {
		return nil, err
	}

	slog.Info("IAM policy attached to role", "accountID", accountID, "roleName", roleName, "policyArn", policyARN)
	return &iam.AttachRolePolicyOutput{}, nil
}

func (s *IAMServiceImpl) DetachRolePolicy(accountID string, input *iam.DetachRolePolicyInput) (*iam.DetachRolePolicyOutput, error) {
	ctx := context.Background()
	roleName := *input.RoleName
	policyARN := *input.PolicyArn

	err := s.updateRoleCAS(ctx, accountID, roleName, func(role *Role) (bool, error) {
		idx := slices.Index(role.AttachedPolicies, policyARN)
		if idx < 0 {
			return false, errors.New(awserrors.ErrorIAMNoSuchEntity)
		}
		role.AttachedPolicies = slices.Delete(role.AttachedPolicies, idx, idx+1)
		return true, nil
	})
	if err != nil {
		return nil, err
	}

	slog.Info("IAM policy detached from role", "accountID", accountID, "roleName", roleName, "policyArn", policyARN)
	return &iam.DetachRolePolicyOutput{}, nil
}

func (s *IAMServiceImpl) ListAttachedRolePolicies(accountID string, input *iam.ListAttachedRolePoliciesInput) (*iam.ListAttachedRolePoliciesOutput, error) {
	ctx := context.Background()
	role, err := s.getRole(ctx, accountID, *input.RoleName)
	if err != nil {
		return nil, err
	}

	var attached []*iam.AttachedPolicy
	for _, arn := range role.AttachedPolicies {
		if isAWSManagedPolicyARN(arn) {
			attached = append(attached, &iam.AttachedPolicy{
				PolicyArn:  aws.String(arn),
				PolicyName: aws.String(managedPolicyNameFromARN(arn)),
			})
			continue
		}
		policy, err := s.getPolicyByARN(ctx, accountID, arn)
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

// PutRolePolicy embeds an inline policy document in a role, keyed by PolicyName.
// Idempotent upsert: a same-name policy is overwritten, mirroring AWS.
func (s *IAMServiceImpl) PutRolePolicy(accountID string, input *iam.PutRolePolicyInput) (*iam.PutRolePolicyOutput, error) {
	ctx := context.Background()
	roleName := *input.RoleName
	policyName := *input.PolicyName
	policyDoc := *input.PolicyDocument

	if err := validatePolicyName(policyName); err != nil {
		return nil, errors.New(awserrors.ErrorIAMInvalidInput)
	}
	if _, err := ValidatePolicyDocument(policyDoc); err != nil {
		return nil, errors.New(awserrors.ErrorIAMMalformedPolicyDocument)
	}

	err := s.updateRoleCAS(ctx, accountID, roleName, func(role *Role) (bool, error) {
		if role.InlinePolicies == nil {
			role.InlinePolicies = map[string]string{}
		}
		role.InlinePolicies[policyName] = policyDoc
		return true, nil
	})
	if err != nil {
		return nil, err
	}

	slog.Info("IAM inline policy put on role", "accountID", accountID, "roleName", roleName, "policyName", policyName)
	return &iam.PutRolePolicyOutput{}, nil
}

// GetRolePolicy returns a role's inline policy document by name.
// Returns the document as a raw JSON string, matching how GetRole returns
// AssumeRolePolicyDocument; AWS URL-encodes it, we follow the in-repo convention.
func (s *IAMServiceImpl) GetRolePolicy(accountID string, input *iam.GetRolePolicyInput) (*iam.GetRolePolicyOutput, error) {
	ctx := context.Background()
	roleName := *input.RoleName
	policyName := *input.PolicyName

	role, err := s.getRole(ctx, accountID, roleName)
	if err != nil {
		return nil, err
	}

	doc, ok := role.InlinePolicies[policyName]
	if !ok {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}

	return &iam.GetRolePolicyOutput{
		RoleName:       aws.String(roleName),
		PolicyName:     aws.String(policyName),
		PolicyDocument: aws.String(doc),
	}, nil
}

// DeleteRolePolicy removes a role's inline policy by name. A missing name yields
// NoSuchEntity, matching AWS.
func (s *IAMServiceImpl) DeleteRolePolicy(accountID string, input *iam.DeleteRolePolicyInput) (*iam.DeleteRolePolicyOutput, error) {
	ctx := context.Background()
	roleName := *input.RoleName
	policyName := *input.PolicyName

	err := s.updateRoleCAS(ctx, accountID, roleName, func(role *Role) (bool, error) {
		if _, ok := role.InlinePolicies[policyName]; !ok {
			return false, errors.New(awserrors.ErrorIAMNoSuchEntity)
		}
		delete(role.InlinePolicies, policyName)
		return true, nil
	})
	if err != nil {
		return nil, err
	}

	slog.Info("IAM inline policy deleted from role", "accountID", accountID, "roleName", roleName, "policyName", policyName)
	return &iam.DeleteRolePolicyOutput{}, nil
}

// ListRolePolicies returns the names of a role's inline policies, sorted for
// deterministic output. Pagination is not implemented: IsTruncated is always false.
func (s *IAMServiceImpl) ListRolePolicies(accountID string, input *iam.ListRolePoliciesInput) (*iam.ListRolePoliciesOutput, error) {
	ctx := context.Background()
	role, err := s.getRole(ctx, accountID, *input.RoleName)
	if err != nil {
		return nil, err
	}

	rawNames := slices.Sorted(maps.Keys(role.InlinePolicies))

	names := make([]*string, 0, len(rawNames))
	for _, name := range rawNames {
		names = append(names, aws.String(name))
	}

	return &iam.ListRolePoliciesOutput{
		PolicyNames: names,
		IsTruncated: aws.Bool(false),
	}, nil
}

// TagRole upserts tags on a role under CAS, like the other role writers.
func (s *IAMServiceImpl) TagRole(accountID string, input *iam.TagRoleInput) (*iam.TagRoleOutput, error) {
	ctx := context.Background()
	if err := validateTags(input.Tags); err != nil {
		return nil, err
	}

	roleName := *input.RoleName
	err := s.updateRoleCAS(ctx, accountID, roleName, func(role *Role) (bool, error) {
		merged := mergeTags(role.Tags, input.Tags)
		if len(merged) > maxTagsPerResource {
			return false, errors.New(awserrors.ErrorIAMLimitExceeded)
		}
		role.Tags = merged
		return true, nil
	})
	if err != nil {
		return nil, err
	}

	slog.Info("IAM role tagged", "accountID", accountID, "roleName", roleName)
	return &iam.TagRoleOutput{}, nil
}

// UntagRole removes the named tag keys from a role; unknown keys are a no-op.
func (s *IAMServiceImpl) UntagRole(accountID string, input *iam.UntagRoleInput) (*iam.UntagRoleOutput, error) {
	ctx := context.Background()
	roleName := *input.RoleName
	err := s.updateRoleCAS(ctx, accountID, roleName, func(role *Role) (bool, error) {
		role.Tags = removeTagKeys(role.Tags, input.TagKeys)
		return true, nil
	})
	if err != nil {
		return nil, err
	}

	slog.Info("IAM role untagged", "accountID", accountID, "roleName", roleName)
	return &iam.UntagRoleOutput{}, nil
}

// ListRoleTags returns a role's tags. Pagination is not implemented:
// IsTruncated is always false.
func (s *IAMServiceImpl) ListRoleTags(accountID string, input *iam.ListRoleTagsInput) (*iam.ListRoleTagsOutput, error) {
	ctx := context.Background()
	role, err := s.getRole(ctx, accountID, *input.RoleName)
	if err != nil {
		return nil, err
	}
	return &iam.ListRoleTagsOutput{
		Tags:        tagsToSDK(role.Tags),
		IsTruncated: aws.Bool(false),
	}, nil
}

// GetRolePolicies resolves the managed and inline policy documents for a role.
// Used by the gateway for policy evaluation. Fails closed: any unresolvable
// policy returns an error so the caller denies access rather than using a partial set.
func (s *IAMServiceImpl) GetRolePolicies(accountID, roleName string) ([]PolicyDocument, error) {
	ctx := context.Background()
	role, err := s.getRole(ctx, accountID, roleName)
	if err != nil {
		return nil, err
	}

	var docs []PolicyDocument
	for _, arn := range role.AttachedPolicies {
		doc, include, err := s.resolveAttachedPolicy(ctx, accountID, arn)
		if err != nil {
			return nil, err // fail closed
		}
		if include {
			docs = append(docs, doc)
		}
	}

	for name, raw := range role.InlinePolicies {
		var doc PolicyDocument
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			return nil, fmt.Errorf("parse inline policy %s: %w", name, err) // fail closed
		}
		docs = append(docs, doc)
	}

	return docs, nil
}

// isAWSManagedPolicyARN reports whether arn is an AWS-managed policy ARN
// (arn:aws:iam::aws:policy/...). These are not provisioned in Spinifex but are
// stored and round-tripped opaquely so stock EKS tooling that attaches them
// works without a backing policy document.
func isAWSManagedPolicyARN(arn string) bool {
	return strings.HasPrefix(arn, "arn:aws:iam::aws:policy/")
}

// managedPolicyNameFromARN returns the final path segment of an AWS-managed
// policy ARN, e.g. .../service-role/AmazonEKS_CNI_Policy -> AmazonEKS_CNI_Policy.
func managedPolicyNameFromARN(arn string) string {
	name := strings.TrimPrefix(arn, "arn:aws:iam::aws:policy/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return name
}

func (s *IAMServiceImpl) getRole(ctx context.Context, accountID, roleName string) (*Role, error) {
	entry, err := s.rolesBucket.Get(ctx, accountID+"."+roleName)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
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

// updateRoleCAS applies mutate to a role under optimistic concurrency: read the
// record with its revision, mutate, then Update guarded by that revision,
// retrying when a concurrent writer wins the race. A blind read-modify-Put
// loses updates when callers (e.g. Terraform attaching several managed policies
// to one role at once) write the same record concurrently. mutate reports
// whether it changed the record; a false return commits nothing.
func (s *IAMServiceImpl) updateRoleCAS(ctx context.Context, accountID, roleName string, mutate func(*Role) (bool, error)) error {
	key := accountID + "." + roleName
	for range roleCASMaxRetries {
		entry, err := s.rolesBucket.Get(ctx, key)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				return errors.New(awserrors.ErrorIAMNoSuchEntity)
			}
			return fmt.Errorf("get role: %w", err)
		}

		var role Role
		if err := json.Unmarshal(entry.Value(), &role); err != nil {
			return fmt.Errorf("unmarshal role: %w", err)
		}

		changed, err := mutate(&role)
		if err != nil {
			return err
		}
		if !changed {
			return nil
		}

		data, err := json.Marshal(&role)
		if err != nil {
			return fmt.Errorf("marshal role: %w", err)
		}
		if _, err := s.rolesBucket.Update(ctx, key, data, entry.Revision()); err != nil {
			if errors.Is(err, jetstream.ErrKeyExists) {
				continue // CAS conflict — another writer won, re-read and retry.
			}
			return fmt.Errorf("update role: %w", err)
		}
		return nil
	}
	return errors.New(awserrors.ErrorServerInternal)
}

// findInstanceProfilesForRole scans the instance-profiles bucket for any
// profile in the account that references the given role. Fails closed on
// per-key Get or unmarshal errors so DeleteRole cannot succeed while a
// real-but-unreadable reference exists.
func (s *IAMServiceImpl) findInstanceProfilesForRole(ctx context.Context, accountID, roleName string) ([]*InstanceProfile, error) {
	keys, err := kvutil.Keys(ctx, s.instanceProfilesBucket)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
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

		entry, err := s.instanceProfilesBucket.Get(ctx, key)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
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

// STSActionAssumeRoleWithWebIdentity is the only action that may carry a Condition block.
const STSActionAssumeRoleWithWebIdentity = "sts:AssumeRoleWithWebIdentity"

// ValidateTrustPolicyDocument parses and validates an AssumeRolePolicyDocument JSON string.
// Rejects NotPrincipal and NotAction at write time. Condition blocks are accepted only for
// sts:AssumeRoleWithWebIdentity with StringEquals — any wider shape would be silently ignored at runtime.
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
			if err := validateWebIdentityCondition(i, stmt); err != nil {
				return nil, err
			}
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

// validateWebIdentityCondition gates Condition blocks to the IRSA case:
// Action must be exactly sts:AssumeRoleWithWebIdentity and operator must be StringEquals only.
func validateWebIdentityCondition(i int, stmt TrustStatement) error {
	if len(stmt.Action) != 1 || stmt.Action[0] != STSActionAssumeRoleWithWebIdentity {
		return fmt.Errorf("statement %d: trust policy Condition blocks are only supported on statements whose Action is exactly [%q]; v1 does not evaluate conditions for other actions", i, STSActionAssumeRoleWithWebIdentity)
	}
	var ops map[string]json.RawMessage
	if err := json.Unmarshal(stmt.Condition, &ops); err != nil {
		return fmt.Errorf("statement %d: Condition must be a JSON object: %w", i, err)
	}
	for op := range ops {
		if op != "StringEquals" {
			return fmt.Errorf("statement %d: Condition operator %q is not supported in this release; only StringEquals is supported", i, op)
		}
	}
	return nil
}

// isRawJSONNonEmpty reports whether a json.RawMessage carries a meaningful value.
// Whitespace, null, and empty object/array all count as empty.
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
