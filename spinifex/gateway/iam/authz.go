// Package gateway_iam contains IAM request validation and authorization helpers.
package gateway_iam

import (
	"errors"
	"reflect"
	"sort"

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
	actions := make([]string, 0, len(iamScopes))
	for action := range iamScopes {
		actions = append(actions, action)
	}
	sort.Strings(actions)
	return actions
}

// ResourceARNs resolves the resource an IAM request authorizes against from
// the same parsed SDK input that dispatch receives.
func ResourceARNs(action, accountID string, input any, svc handlers_iam.IAMService) ([]string, error) {
	scope, ok := iamScopes[action]
	if !ok {
		return nil, errors.New(awserrors.ErrorInvalidAction)
	}
	resource, err := scope.resolve(accountID, input, svc)
	if err != nil {
		return nil, err
	}
	if resource == "" {
		resource = anyResource
	}
	return []string{resource}, nil
}

func (s resourceScope) resolve(accountID string, input any, svc handlers_iam.IAMService) (string, error) {
	if s.source == sourceAccount {
		return anyResource, nil
	}
	name, err := stringField(input, s.nameField)
	if err != nil {
		return "", err
	}
	if name == "" {
		return anyResource, nil
	}

	switch s.source {
	case sourceCreate:
		resourcePath, err := stringField(input, s.pathField)
		if err != nil {
			return "", err
		}
		return arn.FormatIAMPath(s.kind, accountID, resourcePath, name), nil
	case sourceExisting:
		return canonicalARN(accountID, s.kind, name, svc)
	case sourcePolicyARN:
		return name, nil
	case sourceOIDCCreate:
		hostPath, _ := handlers_iam.OIDCProviderHostPathFromURL(name)
		if hostPath == "" {
			return anyResource, nil
		}
		return arn.FormatIAMResource(arn.IAMOIDCProvider, accountID, hostPath), nil
	case sourceOIDCARN:
		hostPath, _ := handlers_iam.OIDCProviderHostPathFromARN(name)
		if hostPath == "" {
			return anyResource, nil
		}
		return arn.FormatIAMResource(arn.IAMOIDCProvider, accountID, hostPath), nil
	case sourceAccessKeyOwner:
		key, err := svc.LookupAccessKey(name)
		if err != nil {
			return lookupResult("", err)
		}
		if key == nil || key.AccountID != accountID || key.UserName == "" {
			return anyResource, nil
		}
		return canonicalARN(accountID, arn.IAMUser, key.UserName, svc)
	default:
		return "", errors.New(awserrors.ErrorInternalError)
	}
}

func canonicalARN(accountID string, kind arn.IAMResourceType, name string, svc handlers_iam.IAMService) (string, error) {
	resource, err := svc.CanonicalResourceARN(accountID, kind, name)
	return lookupResult(resource, err)
}

func lookupResult(resource string, err error) (string, error) {
	if err == nil {
		if resource == "" {
			return "", errors.New(awserrors.ErrorInternalError)
		}
		return resource, nil
	}
	if err.Error() == awserrors.ErrorIAMNoSuchEntity {
		return anyResource, nil
	}
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
