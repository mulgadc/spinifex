// Package gateway_iam contains IAM request validation and authorization helpers.
package gateway_iam

import (
	"errors"
	"log/slog"
	"maps"
	"reflect"
	"slices"

	"github.com/mulgadc/spinifex/spinifex/arn"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
)

const anyResource = "*"

type resourceSource uint8

const (
	sourceAccount resourceSource = iota
	sourceCreate
	sourceExisting
	sourcePolicyARN
	sourceOIDCCreate
	sourceOIDCARN
	sourceAccessKeyOwner
)

type resourceScope struct {
	source    resourceSource
	kind      arn.IAMResourceType
	nameField string
	pathField string
}

func createScope(kind arn.IAMResourceType, name string) resourceScope {
	return resourceScope{source: sourceCreate, kind: kind, nameField: name, pathField: "Path"}
}

func existingScope(kind arn.IAMResourceType, name string) resourceScope {
	return resourceScope{source: sourceExisting, kind: kind, nameField: name}
}

var iamScopes = map[string]resourceScope{
	// Users.
	"CreateUser":               createScope(arn.IAMUser, "UserName"),
	"GetUser":                  existingScope(arn.IAMUser, "UserName"),
	"DeleteUser":               existingScope(arn.IAMUser, "UserName"),
	"CreateAccessKey":          existingScope(arn.IAMUser, "UserName"),
	"ListAccessKeys":           existingScope(arn.IAMUser, "UserName"),
	"DeleteAccessKey":          existingScope(arn.IAMUser, "UserName"),
	"UpdateAccessKey":          {source: sourceAccessKeyOwner, nameField: "AccessKeyId"},
	"AttachUserPolicy":         existingScope(arn.IAMUser, "UserName"),
	"DetachUserPolicy":         existingScope(arn.IAMUser, "UserName"),
	"ListAttachedUserPolicies": existingScope(arn.IAMUser, "UserName"),
	"PutUserPolicy":            existingScope(arn.IAMUser, "UserName"),
	"GetUserPolicy":            existingScope(arn.IAMUser, "UserName"),
	"DeleteUserPolicy":         existingScope(arn.IAMUser, "UserName"),
	"ListUserPolicies":         existingScope(arn.IAMUser, "UserName"),
	"TagUser":                  existingScope(arn.IAMUser, "UserName"),
	"UntagUser":                existingScope(arn.IAMUser, "UserName"),
	"ListUserTags":             existingScope(arn.IAMUser, "UserName"),
	"ListGroupsForUser":        existingScope(arn.IAMUser, "UserName"),

	// Roles.
	"CreateRole":                  createScope(arn.IAMRole, "RoleName"),
	"GetRole":                     existingScope(arn.IAMRole, "RoleName"),
	"DeleteRole":                  existingScope(arn.IAMRole, "RoleName"),
	"UpdateRole":                  existingScope(arn.IAMRole, "RoleName"),
	"UpdateAssumeRolePolicy":      existingScope(arn.IAMRole, "RoleName"),
	"AttachRolePolicy":            existingScope(arn.IAMRole, "RoleName"),
	"DetachRolePolicy":            existingScope(arn.IAMRole, "RoleName"),
	"PutRolePolicy":               existingScope(arn.IAMRole, "RoleName"),
	"GetRolePolicy":               existingScope(arn.IAMRole, "RoleName"),
	"DeleteRolePolicy":            existingScope(arn.IAMRole, "RoleName"),
	"ListAttachedRolePolicies":    existingScope(arn.IAMRole, "RoleName"),
	"ListRolePolicies":            existingScope(arn.IAMRole, "RoleName"),
	"TagRole":                     existingScope(arn.IAMRole, "RoleName"),
	"UntagRole":                   existingScope(arn.IAMRole, "RoleName"),
	"ListRoleTags":                existingScope(arn.IAMRole, "RoleName"),
	"ListInstanceProfilesForRole": existingScope(arn.IAMRole, "RoleName"),

	// Groups. Membership actions evaluate only the group.
	"CreateGroup":               createScope(arn.IAMGroup, "GroupName"),
	"GetGroup":                  existingScope(arn.IAMGroup, "GroupName"),
	"DeleteGroup":               existingScope(arn.IAMGroup, "GroupName"),
	"AddUserToGroup":            existingScope(arn.IAMGroup, "GroupName"),
	"RemoveUserFromGroup":       existingScope(arn.IAMGroup, "GroupName"),
	"AttachGroupPolicy":         existingScope(arn.IAMGroup, "GroupName"),
	"DetachGroupPolicy":         existingScope(arn.IAMGroup, "GroupName"),
	"ListAttachedGroupPolicies": existingScope(arn.IAMGroup, "GroupName"),
	"PutGroupPolicy":            existingScope(arn.IAMGroup, "GroupName"),
	"GetGroupPolicy":            existingScope(arn.IAMGroup, "GroupName"),
	"DeleteGroupPolicy":         existingScope(arn.IAMGroup, "GroupName"),
	"ListGroupPolicies":         existingScope(arn.IAMGroup, "GroupName"),

	// Managed policies use the exact ARN the handler requires.
	"CreatePolicy":       createScope(arn.IAMPolicy, "PolicyName"),
	"GetPolicy":          {source: sourcePolicyARN, nameField: "PolicyArn"},
	"GetPolicyVersion":   {source: sourcePolicyARN, nameField: "PolicyArn"},
	"ListPolicyVersions": {source: sourcePolicyARN, nameField: "PolicyArn"},
	"DeletePolicy":       {source: sourcePolicyARN, nameField: "PolicyArn"},
	"TagPolicy":          {source: sourcePolicyARN, nameField: "PolicyArn"},
	"UntagPolicy":        {source: sourcePolicyARN, nameField: "PolicyArn"},
	"ListPolicyTags":     {source: sourcePolicyARN, nameField: "PolicyArn"},

	// Instance profiles. Role operands are IAM condition-key values, not extra resources.
	"CreateInstanceProfile":         createScope(arn.IAMInstanceProfile, "InstanceProfileName"),
	"GetInstanceProfile":            existingScope(arn.IAMInstanceProfile, "InstanceProfileName"),
	"DeleteInstanceProfile":         existingScope(arn.IAMInstanceProfile, "InstanceProfileName"),
	"AddRoleToInstanceProfile":      existingScope(arn.IAMInstanceProfile, "InstanceProfileName"),
	"RemoveRoleFromInstanceProfile": existingScope(arn.IAMInstanceProfile, "InstanceProfileName"),
	"TagInstanceProfile":            existingScope(arn.IAMInstanceProfile, "InstanceProfileName"),
	"UntagInstanceProfile":          existingScope(arn.IAMInstanceProfile, "InstanceProfileName"),
	"ListInstanceProfileTags":       existingScope(arn.IAMInstanceProfile, "InstanceProfileName"),

	// OIDC providers are normalized into the caller's trusted account.
	"CreateOpenIDConnectProvider":   {source: sourceOIDCCreate, nameField: "Url"},
	"GetOpenIDConnectProvider":      {source: sourceOIDCARN, nameField: "OpenIDConnectProviderArn"},
	"DeleteOpenIDConnectProvider":   {source: sourceOIDCARN, nameField: "OpenIDConnectProviderArn"},
	"TagOpenIDConnectProvider":      {source: sourceOIDCARN, nameField: "OpenIDConnectProviderArn"},
	"UntagOpenIDConnectProvider":    {source: sourceOIDCARN, nameField: "OpenIDConnectProviderArn"},
	"ListOpenIDConnectProviderTags": {source: sourceOIDCARN, nameField: "OpenIDConnectProviderArn"},

	// Account-wide actions are explicit entries, not omissions.
	"ListUsers":                  {source: sourceAccount},
	"ListPolicies":               {source: sourceAccount},
	"ListRoles":                  {source: sourceAccount},
	"ListInstanceProfiles":       {source: sourceAccount},
	"ListOpenIDConnectProviders": {source: sourceAccount},
	"ListGroups":                 {source: sourceAccount},
	"GetAccountSummary":          {source: sourceAccount},
}

// HasScope reports whether action has an explicit IAM scope-table entry.
func HasScope(action string) bool {
	_, ok := iamScopes[action]
	return ok
}

// ScopedActions returns every action represented in the IAM scope table.
func ScopedActions() []string {
	return slices.Sorted(maps.Keys(iamScopes))
}

// ResourceARNs resolves the resource an IAM request authorizes against from
// the same parsed SDK input that dispatch receives.
func ResourceARNs(action, accountID string, input any, svc handlers_iam.IAMService) ([]string, error) {
	scope, ok := iamScopes[action]
	if !ok {
		slog.Error("IAM authz: action is served but absent from the scope table", "action", action)
		return nil, errors.New(awserrors.ErrorInvalidAction)
	}
	resource, err := scope.resolve(action, accountID, input, svc)
	if err != nil {
		return nil, err
	}
	if resource == "" {
		resource = anyResource
	}
	return []string{resource}, nil
}

func (s resourceScope) resolve(action, accountID string, input any, svc handlers_iam.IAMService) (string, error) {
	if s.source == sourceAccount {
		return anyResource, nil
	}
	target := lookupTarget{action: action, kind: string(s.kind), accountID: accountID}
	name, err := stringField(input, s.nameField)
	if err != nil {
		slog.Error("IAM authz: scope field is unreadable on the handler input, failing closed",
			"action", action, "field", s.nameField, "err", err)
		return "", err
	}
	target.name = name
	if name == "" {
		// The handler rejects the request as a validation fault, so this
		// widening cannot reach a mutation. Logged at Debug, not Warn.
		slog.Debug("IAM authz: identifier absent, authorizing account-wide",
			"action", action, "field", s.nameField, "account_id", accountID)
		return anyResource, nil
	}

	switch s.source {
	case sourceCreate:
		resourcePath, err := stringField(input, s.pathField)
		if err != nil {
			slog.Error("IAM authz: path field is unreadable on the handler input, failing closed",
				"action", action, "field", s.pathField, "err", err)
			return "", err
		}
		return arn.FormatIAMPath(s.kind, accountID, resourcePath, name), nil
	case sourceExisting:
		return canonicalARN(target, s.kind, svc)
	case sourcePolicyARN:
		return name, nil
	case sourceOIDCCreate:
		hostPath, err := handlers_iam.OIDCProviderHostPathFromURL(name)
		if hostPath == "" {
			slog.Warn("IAM authz: issuer URL unresolvable, authorizing account-wide",
				"action", action, "url", name, "account_id", accountID, "err", err)
			return anyResource, nil
		}
		return arn.FormatIAMResource(arn.IAMOIDCProvider, accountID, hostPath), nil
	case sourceOIDCARN:
		hostPath, err := handlers_iam.OIDCProviderHostPathFromARN(name)
		if hostPath == "" {
			slog.Warn("IAM authz: provider ARN unresolvable, authorizing account-wide",
				"action", action, "provider_arn", name, "account_id", accountID, "err", err)
			return anyResource, nil
		}
		return arn.FormatIAMResource(arn.IAMOIDCProvider, accountID, hostPath), nil
	case sourceAccessKeyOwner:
		target.kind = "access-key"
		key, err := svc.LookupAccessKey(name)
		if err != nil {
			return lookupResult(target, "", err)
		}
		if key == nil || key.AccountID != accountID || key.UserName == "" {
			slog.Warn("IAM authz: access key unknown, cross-account, or ownerless, authorizing account-wide",
				"action", action, "access_key_id", name, "account_id", accountID)
			return anyResource, nil
		}
		target.kind, target.name = string(arn.IAMUser), key.UserName
		return canonicalARN(target, arn.IAMUser, svc)
	default:
		slog.Error("IAM authz: unhandled resource source, failing closed",
			"action", action, "source", s.source)
		return "", errors.New(awserrors.ErrorInternalError)
	}
}

// lookupTarget carries the identity of the object being authorized so every
// resolution outcome can be logged with enough context to act on.
type lookupTarget struct {
	action    string
	kind      string
	name      string
	accountID string
}

func canonicalARN(t lookupTarget, kind arn.IAMResourceType, svc handlers_iam.IAMService) (string, error) {
	resource, err := svc.CanonicalResourceARN(t.accountID, kind, t.name)
	return lookupResult(t, resource, err)
}

// lookupResult widens authorization to account-wide for a missing object and
// fails closed on a storage or decode fault. Both outcomes are logged because
// neither is visible in the response the caller receives.
func lookupResult(t lookupTarget, resource string, err error) (string, error) {
	if err == nil {
		if resource == "" {
			slog.Error("IAM authz: stored record carries no ARN, failing closed",
				"action", t.action, "kind", t.kind, "name", t.name, "account_id", t.accountID)
			return "", errors.New(awserrors.ErrorInternalError)
		}
		return resource, nil
	}
	if err.Error() == awserrors.ErrorIAMNoSuchEntity {
		// Authorization widens to "*", so a Deny scoped to the real target
		// will not fire. Without this line that widening leaves no trace.
		slog.Warn("IAM authz: target not found, authorizing account-wide",
			"action", t.action, "kind", t.kind, "name", t.name, "account_id", t.accountID)
		return anyResource, nil
	}
	slog.Error("IAM authz: canonical ARN lookup failed, failing closed",
		"action", t.action, "kind", t.kind, "name", t.name, "account_id", t.accountID, "err", err)
	return "", errors.New(awserrors.ErrorInternalError)
}

func stringField(input any, name string) (string, error) {
	value := reflect.ValueOf(input)
	for value.IsValid() && value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return "", errors.New(awserrors.ErrorInternalError)
		}
		value = value.Elem()
	}
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return "", errors.New(awserrors.ErrorInternalError)
	}
	field := value.FieldByName(name)
	if !field.IsValid() {
		return "", errors.New(awserrors.ErrorInternalError)
	}
	if field.Kind() == reflect.Pointer {
		if field.IsNil() {
			return "", nil
		}
		field = field.Elem()
	}
	if field.Kind() != reflect.String {
		return "", errors.New(awserrors.ErrorInternalError)
	}
	return field.String(), nil
}
