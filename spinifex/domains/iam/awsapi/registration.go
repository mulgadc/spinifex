package awsapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	"github.com/mulgadc/bluebottle/pkg/sigv4"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	awsxml "github.com/mulgadc/spinifex/spinifex/foundation/aws/xml"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/query"
)

// ServiceName is the SigV4 credential-scope service IAM clients sign for.
const ServiceName = "iam"

const (
	principalTypeUser        = "user"
	principalTypeRoot        = "root"
	principalTypeAssumedRole = "assumed-role"

	// ec2ServicePrincipal is the service an instance profile's role is passed to.
	ec2ServicePrincipal = "ec2.amazonaws.com"
)

// Deps are the composed capabilities the IAM control plane dispatches to. A
// nil IAM answers every action with InternalError.
type Deps struct {
	IAM iamdomain.IAMService
	// CountInstanceProfileAssociations counts the live instances holding the
	// instance profile, so DeleteInstanceProfile can refuse while it is in use.
	CountInstanceProfileAssociations func(ctx context.Context, accountID, profileARN string) (int, error)
}

// action parses once so authorization and dispatch share one typed input.
type action struct {
	parse    func(map[string]string) (any, error)
	dispatch func(name string, parsed any, d Deps, accountID string) ([]byte, error)
}

// handler creates a type-safe IAM action with separate parse and dispatch
// stages so policy resolution consumes the exact handler input.
func handler[In any](h func(string, *In, Deps) (any, error)) action {
	return action{
		parse: func(q map[string]string) (any, error) {
			input := new(In)
			if err := query.QueryParamsToStruct(q, input); err != nil {
				if errors.Is(err, query.ErrSliceTooLarge) {
					return nil, errors.New(awserrors.ErrorMalformedQueryString)
				}
				return nil, awserrors.Errorf(awserrors.ErrorIAMInvalidInput, "The request parameters are invalid: %v", err)
			}
			return input, nil
		},
		dispatch: func(name string, parsed any, d Deps, accountID string) ([]byte, error) {
			input, ok := parsed.(*In)
			if !ok {
				return nil, errors.New(awserrors.ErrorInternalError)
			}
			output, err := h(accountID, input, d)
			if err != nil {
				return nil, err
			}
			payload := awsxml.QueryResponsePayload(name, output)
			xmlOutput, err := awsxml.Marshal(payload)
			if err != nil {
				return nil, errors.New(awserrors.ErrorInternalError)
			}
			return xmlOutput, nil
		},
	}
}

var actions = map[string]action{
	"CreateUser": handler(func(accountID string, input *iam.CreateUserInput, d Deps) (any, error) {
		return CreateUser(accountID, input, d.IAM)
	}),
	"GetUser": handler(func(accountID string, input *iam.GetUserInput, d Deps) (any, error) {
		return GetUser(accountID, input, d.IAM)
	}),
	"ListUsers": handler(func(accountID string, input *iam.ListUsersInput, d Deps) (any, error) {
		return ListUsers(accountID, input, d.IAM)
	}),
	"DeleteUser": handler(func(accountID string, input *iam.DeleteUserInput, d Deps) (any, error) {
		return DeleteUser(accountID, input, d.IAM)
	}),
	"CreateAccessKey": handler(func(accountID string, input *iam.CreateAccessKeyInput, d Deps) (any, error) {
		return CreateAccessKey(accountID, input, d.IAM)
	}),
	"ListAccessKeys": handler(func(accountID string, input *iam.ListAccessKeysInput, d Deps) (any, error) {
		return ListAccessKeys(accountID, input, d.IAM)
	}),
	"DeleteAccessKey": handler(func(accountID string, input *iam.DeleteAccessKeyInput, d Deps) (any, error) {
		return DeleteAccessKey(accountID, input, d.IAM)
	}),
	"UpdateAccessKey": handler(func(accountID string, input *iam.UpdateAccessKeyInput, d Deps) (any, error) {
		return UpdateAccessKey(accountID, input, d.IAM)
	}),

	// Policy CRUD
	"CreatePolicy": handler(func(accountID string, input *iam.CreatePolicyInput, d Deps) (any, error) {
		return CreatePolicy(accountID, input, d.IAM)
	}),
	"GetPolicy": handler(func(accountID string, input *iam.GetPolicyInput, d Deps) (any, error) {
		return GetPolicy(accountID, input, d.IAM)
	}),
	"GetPolicyVersion": handler(func(accountID string, input *iam.GetPolicyVersionInput, d Deps) (any, error) {
		return GetPolicyVersion(accountID, input, d.IAM)
	}),
	"ListPolicyVersions": handler(func(accountID string, input *iam.ListPolicyVersionsInput, d Deps) (any, error) {
		return ListPolicyVersions(accountID, input, d.IAM)
	}),
	"CreatePolicyVersion": handler(func(accountID string, input *iam.CreatePolicyVersionInput, d Deps) (any, error) {
		return CreatePolicyVersion(accountID, input, d.IAM)
	}),
	"SetDefaultPolicyVersion": handler(func(accountID string, input *iam.SetDefaultPolicyVersionInput, d Deps) (any, error) {
		return SetDefaultPolicyVersion(accountID, input, d.IAM)
	}),
	"DeletePolicyVersion": handler(func(accountID string, input *iam.DeletePolicyVersionInput, d Deps) (any, error) {
		return DeletePolicyVersion(accountID, input, d.IAM)
	}),
	"ListPolicies": handler(func(accountID string, input *iam.ListPoliciesInput, d Deps) (any, error) {
		return ListPolicies(accountID, input, d.IAM)
	}),
	"DeletePolicy": handler(func(accountID string, input *iam.DeletePolicyInput, d Deps) (any, error) {
		return DeletePolicy(accountID, input, d.IAM)
	}),
	"ListEntitiesForPolicy": handler(func(accountID string, input *iam.ListEntitiesForPolicyInput, d Deps) (any, error) {
		return ListEntitiesForPolicy(accountID, input, d.IAM)
	}),

	// Policy attachment
	"AttachUserPolicy": handler(func(accountID string, input *iam.AttachUserPolicyInput, d Deps) (any, error) {
		return AttachUserPolicy(accountID, input, d.IAM)
	}),
	"DetachUserPolicy": handler(func(accountID string, input *iam.DetachUserPolicyInput, d Deps) (any, error) {
		return DetachUserPolicy(accountID, input, d.IAM)
	}),
	"ListAttachedUserPolicies": handler(func(accountID string, input *iam.ListAttachedUserPoliciesInput, d Deps) (any, error) {
		return ListAttachedUserPolicies(accountID, input, d.IAM)
	}),

	// User inline policies
	"PutUserPolicy": handler(func(accountID string, input *iam.PutUserPolicyInput, d Deps) (any, error) {
		return PutUserPolicy(accountID, input, d.IAM)
	}),
	"GetUserPolicy": handler(func(accountID string, input *iam.GetUserPolicyInput, d Deps) (any, error) {
		return GetUserPolicy(accountID, input, d.IAM)
	}),
	"DeleteUserPolicy": handler(func(accountID string, input *iam.DeleteUserPolicyInput, d Deps) (any, error) {
		return DeleteUserPolicy(accountID, input, d.IAM)
	}),
	"ListUserPolicies": handler(func(accountID string, input *iam.ListUserPoliciesInput, d Deps) (any, error) {
		return ListUserPolicies(accountID, input, d.IAM)
	}),

	// Role CRUD
	"CreateRole": handler(func(accountID string, input *iam.CreateRoleInput, d Deps) (any, error) {
		return CreateRole(accountID, input, d.IAM)
	}),
	"GetRole": handler(func(accountID string, input *iam.GetRoleInput, d Deps) (any, error) {
		return GetRole(accountID, input, d.IAM)
	}),
	"ListRoles": handler(func(accountID string, input *iam.ListRolesInput, d Deps) (any, error) {
		return ListRoles(accountID, input, d.IAM)
	}),
	"DeleteRole": handler(func(accountID string, input *iam.DeleteRoleInput, d Deps) (any, error) {
		return DeleteRole(accountID, input, d.IAM)
	}),
	"UpdateRole": handler(func(accountID string, input *iam.UpdateRoleInput, d Deps) (any, error) {
		return UpdateRole(accountID, input, d.IAM)
	}),
	"UpdateRoleDescription": handler(func(accountID string, input *iam.UpdateRoleDescriptionInput, d Deps) (any, error) {
		return UpdateRoleDescription(accountID, input, d.IAM)
	}),
	"UpdateAssumeRolePolicy": handler(func(accountID string, input *iam.UpdateAssumeRolePolicyInput, d Deps) (any, error) {
		return UpdateAssumeRolePolicy(accountID, input, d.IAM)
	}),

	// Role policy attachment
	"AttachRolePolicy": handler(func(accountID string, input *iam.AttachRolePolicyInput, d Deps) (any, error) {
		return AttachRolePolicy(accountID, input, d.IAM)
	}),
	"DetachRolePolicy": handler(func(accountID string, input *iam.DetachRolePolicyInput, d Deps) (any, error) {
		return DetachRolePolicy(accountID, input, d.IAM)
	}),
	"PutRolePolicy": handler(func(accountID string, input *iam.PutRolePolicyInput, d Deps) (any, error) {
		return PutRolePolicy(accountID, input, d.IAM)
	}),
	"GetRolePolicy": handler(func(accountID string, input *iam.GetRolePolicyInput, d Deps) (any, error) {
		return GetRolePolicy(accountID, input, d.IAM)
	}),
	"DeleteRolePolicy": handler(func(accountID string, input *iam.DeleteRolePolicyInput, d Deps) (any, error) {
		return DeleteRolePolicy(accountID, input, d.IAM)
	}),
	"ListAttachedRolePolicies": handler(func(accountID string, input *iam.ListAttachedRolePoliciesInput, d Deps) (any, error) {
		return ListAttachedRolePolicies(accountID, input, d.IAM)
	}),
	"ListRolePolicies": handler(func(accountID string, input *iam.ListRolePoliciesInput, d Deps) (any, error) {
		return ListRolePolicies(accountID, input, d.IAM)
	}),

	// Instance profile CRUD
	"CreateInstanceProfile": handler(func(accountID string, input *iam.CreateInstanceProfileInput, d Deps) (any, error) {
		return CreateInstanceProfile(accountID, input, d.IAM)
	}),
	"GetInstanceProfile": handler(func(accountID string, input *iam.GetInstanceProfileInput, d Deps) (any, error) {
		return GetInstanceProfile(accountID, input, d.IAM)
	}),
	"ListInstanceProfiles": handler(func(accountID string, input *iam.ListInstanceProfilesInput, d Deps) (any, error) {
		return ListInstanceProfiles(accountID, input, d.IAM)
	}),
	"DeleteInstanceProfile": handler(func(accountID string, input *iam.DeleteInstanceProfileInput, d Deps) (any, error) {
		countLive := func(profileARN string) (int, error) {
			if d.CountInstanceProfileAssociations == nil {
				return 0, errors.New(awserrors.ErrorInternalError)
			}
			return d.CountInstanceProfileAssociations(context.Background(), accountID, profileARN)
		}
		return DeleteInstanceProfile(accountID, input, d.IAM, countLive)
	}),
	"ListInstanceProfilesForRole": handler(func(accountID string, input *iam.ListInstanceProfilesForRoleInput, d Deps) (any, error) {
		return ListInstanceProfilesForRole(accountID, input, d.IAM)
	}),

	// Instance profile ↔ role binding
	"AddRoleToInstanceProfile": handler(func(accountID string, input *iam.AddRoleToInstanceProfileInput, d Deps) (any, error) {
		return AddRoleToInstanceProfile(accountID, input, d.IAM)
	}),
	"RemoveRoleFromInstanceProfile": handler(func(accountID string, input *iam.RemoveRoleFromInstanceProfileInput, d Deps) (any, error) {
		return RemoveRoleFromInstanceProfile(accountID, input, d.IAM)
	}),

	// OIDC identity-provider registry (IRSA)
	"CreateOpenIDConnectProvider": handler(func(accountID string, input *iam.CreateOpenIDConnectProviderInput, d Deps) (any, error) {
		return CreateOpenIDConnectProvider(accountID, input, d.IAM)
	}),
	"GetOpenIDConnectProvider": handler(func(accountID string, input *iam.GetOpenIDConnectProviderInput, d Deps) (any, error) {
		return GetOpenIDConnectProvider(accountID, input, d.IAM)
	}),
	"ListOpenIDConnectProviders": handler(func(accountID string, input *iam.ListOpenIDConnectProvidersInput, d Deps) (any, error) {
		return ListOpenIDConnectProviders(accountID, input, d.IAM)
	}),
	"DeleteOpenIDConnectProvider": handler(func(accountID string, input *iam.DeleteOpenIDConnectProviderInput, d Deps) (any, error) {
		return DeleteOpenIDConnectProvider(accountID, input, d.IAM)
	}),

	// Resource tagging
	"TagUser": handler(func(accountID string, input *iam.TagUserInput, d Deps) (any, error) {
		return TagUser(accountID, input, d.IAM)
	}),
	"UntagUser": handler(func(accountID string, input *iam.UntagUserInput, d Deps) (any, error) {
		return UntagUser(accountID, input, d.IAM)
	}),
	"ListUserTags": handler(func(accountID string, input *iam.ListUserTagsInput, d Deps) (any, error) {
		return ListUserTags(accountID, input, d.IAM)
	}),
	"TagRole": handler(func(accountID string, input *iam.TagRoleInput, d Deps) (any, error) {
		return TagRole(accountID, input, d.IAM)
	}),
	"UntagRole": handler(func(accountID string, input *iam.UntagRoleInput, d Deps) (any, error) {
		return UntagRole(accountID, input, d.IAM)
	}),
	"ListRoleTags": handler(func(accountID string, input *iam.ListRoleTagsInput, d Deps) (any, error) {
		return ListRoleTags(accountID, input, d.IAM)
	}),
	"TagPolicy": handler(func(accountID string, input *iam.TagPolicyInput, d Deps) (any, error) {
		return TagPolicy(accountID, input, d.IAM)
	}),
	"UntagPolicy": handler(func(accountID string, input *iam.UntagPolicyInput, d Deps) (any, error) {
		return UntagPolicy(accountID, input, d.IAM)
	}),
	"ListPolicyTags": handler(func(accountID string, input *iam.ListPolicyTagsInput, d Deps) (any, error) {
		return ListPolicyTags(accountID, input, d.IAM)
	}),
	"TagInstanceProfile": handler(func(accountID string, input *iam.TagInstanceProfileInput, d Deps) (any, error) {
		return TagInstanceProfile(accountID, input, d.IAM)
	}),
	"UntagInstanceProfile": handler(func(accountID string, input *iam.UntagInstanceProfileInput, d Deps) (any, error) {
		return UntagInstanceProfile(accountID, input, d.IAM)
	}),
	"ListInstanceProfileTags": handler(func(accountID string, input *iam.ListInstanceProfileTagsInput, d Deps) (any, error) {
		return ListInstanceProfileTags(accountID, input, d.IAM)
	}),
	"TagOpenIDConnectProvider": handler(func(accountID string, input *iam.TagOpenIDConnectProviderInput, d Deps) (any, error) {
		return TagOpenIDConnectProvider(accountID, input, d.IAM)
	}),
	"UntagOpenIDConnectProvider": handler(func(accountID string, input *iam.UntagOpenIDConnectProviderInput, d Deps) (any, error) {
		return UntagOpenIDConnectProvider(accountID, input, d.IAM)
	}),
	"ListOpenIDConnectProviderTags": handler(func(accountID string, input *iam.ListOpenIDConnectProviderTagsInput, d Deps) (any, error) {
		return ListOpenIDConnectProviderTags(accountID, input, d.IAM)
	}),

	// Group CRUD
	"CreateGroup": handler(func(accountID string, input *iam.CreateGroupInput, d Deps) (any, error) {
		return CreateGroup(accountID, input, d.IAM)
	}),
	"GetGroup": handler(func(accountID string, input *iam.GetGroupInput, d Deps) (any, error) {
		return GetGroup(accountID, input, d.IAM)
	}),
	"ListGroups": handler(func(accountID string, input *iam.ListGroupsInput, d Deps) (any, error) {
		return ListGroups(accountID, input, d.IAM)
	}),
	"DeleteGroup": handler(func(accountID string, input *iam.DeleteGroupInput, d Deps) (any, error) {
		return DeleteGroup(accountID, input, d.IAM)
	}),

	// Group membership
	"AddUserToGroup": handler(func(accountID string, input *iam.AddUserToGroupInput, d Deps) (any, error) {
		return AddUserToGroup(accountID, input, d.IAM)
	}),
	"RemoveUserFromGroup": handler(func(accountID string, input *iam.RemoveUserFromGroupInput, d Deps) (any, error) {
		return RemoveUserFromGroup(accountID, input, d.IAM)
	}),
	"ListGroupsForUser": handler(func(accountID string, input *iam.ListGroupsForUserInput, d Deps) (any, error) {
		return ListGroupsForUser(accountID, input, d.IAM)
	}),

	// Group policy attachment
	"AttachGroupPolicy": handler(func(accountID string, input *iam.AttachGroupPolicyInput, d Deps) (any, error) {
		return AttachGroupPolicy(accountID, input, d.IAM)
	}),
	"DetachGroupPolicy": handler(func(accountID string, input *iam.DetachGroupPolicyInput, d Deps) (any, error) {
		return DetachGroupPolicy(accountID, input, d.IAM)
	}),
	"ListAttachedGroupPolicies": handler(func(accountID string, input *iam.ListAttachedGroupPoliciesInput, d Deps) (any, error) {
		return ListAttachedGroupPolicies(accountID, input, d.IAM)
	}),

	// Group inline policies
	"PutGroupPolicy": handler(func(accountID string, input *iam.PutGroupPolicyInput, d Deps) (any, error) {
		return PutGroupPolicy(accountID, input, d.IAM)
	}),
	"GetGroupPolicy": handler(func(accountID string, input *iam.GetGroupPolicyInput, d Deps) (any, error) {
		return GetGroupPolicy(accountID, input, d.IAM)
	}),
	"DeleteGroupPolicy": handler(func(accountID string, input *iam.DeleteGroupPolicyInput, d Deps) (any, error) {
		return DeleteGroupPolicy(accountID, input, d.IAM)
	}),
	"ListGroupPolicies": handler(func(accountID string, input *iam.ListGroupPoliciesInput, d Deps) (any, error) {
		return ListGroupPolicies(accountID, input, d.IAM)
	}),

	// Account
	"GetAccountSummary": handler(func(accountID string, input *iam.GetAccountSummaryInput, d Deps) (any, error) {
		return GetAccountSummary(accountID, input, d.IAM)
	}),
	"ListAccountAliases": handler(func(accountID string, input *iam.ListAccountAliasesInput, d Deps) (any, error) {
		return ListAccountAliases(accountID, input, d.IAM)
	}),
}

// getRootUser answers GetUser without UserName for root, which has no IAM user
// record of the shape AWS returns.
var getRootUser = handler(func(accountID string, _ *iam.GetUserInput, d Deps) (any, error) {
	return GetRootUser(accountID, d.IAM)
})

// NewRegistration returns the IAM AWS Query control-plane registration.
func NewRegistration(deps Deps) dispatch.Registration {
	return dispatch.Registration{
		Service:       ServiceName,
		ResolveAction: resolveAction,
		Dispatch:      deps.dispatch,
		Errors:        dispatch.ErrorEnvelopeXML,
		Inventory:     OperationInventory(),
	}
}

// ActionNames lists every action the registration dispatches, sorted.
func ActionNames() []string {
	return slices.Sorted(maps.Keys(actions))
}

// OperationInventory classifies the actions the registration serves. Every IAM
// action is implemented, so none is Stubbed or Unsupported.
func OperationInventory() dispatch.Inventory {
	return dispatch.Inventory{Registered: ActionNames()}
}

// resolveAction is the one Query Action parser telemetry and dispatch share.
// It leaves the body readable for the dispatcher and answers "" on any failure.
func resolveAction(r *http.Request) string {
	args, err := readQueryArgs(r)
	if err != nil {
		return ""
	}
	return args["Action"]
}

func readQueryArgs(r *http.Request) (map[string]string, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, sigv4.MaxPayloadLen+1))
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if int64(len(body)) > sigv4.MaxPayloadLen {
		return nil, errors.New(awserrors.ErrorRequestEntityTooLarge)
	}
	return query.ParseArgs(string(body))
}

// dispatch parses, scopes and authorizes the request from one parse of its
// body, then runs the action. Errors are awserrors codes the gateway renders.
func (d Deps) dispatch(w http.ResponseWriter, inv dispatch.Invocation) error {
	r := inv.Request
	queryArgs, err := readQueryArgs(r)
	if err != nil {
		slog.DebugContext(r.Context(), "IAM: malformed query string", "err", err)
		return errors.New(awserrors.ErrorMalformedQueryString)
	}

	name := queryArgs["Action"]
	if name == "" {
		return errors.New(awserrors.ErrorMissingAction)
	}
	h, ok := actions[name]
	if !ok {
		slog.DebugContext(r.Context(), "IAM: unknown action", "action", name)
		return errors.New(awserrors.ErrorInvalidAction)
	}

	if d.IAM == nil {
		slog.ErrorContext(r.Context(), "IAM: service not initialized")
		return errors.New(awserrors.ErrorInternalError)
	}

	accountID := inv.AccountID
	if accountID == "" {
		slog.ErrorContext(r.Context(), "IAM dispatch: no account ID in auth context")
		return errors.New(awserrors.ErrorInternalError)
	}

	input, err := h.parse(queryArgs)
	if err != nil {
		return err
	}
	if in, ok := input.(*iam.GetUserInput); ok && aws.StringValue(in.UserName) == "" {
		if h, err = getUserForCaller(r.Context(), inv, accountID, in); err != nil {
			return err
		}
	}
	resources, err := ResourceARNs(name, accountID, input, d.IAM)
	if err != nil {
		return err
	}
	if err := inv.Authorize(ServiceName, name, resources, nil); err != nil {
		return err
	}
	if in, ok := input.(*iam.AddRoleToInstanceProfileInput); ok {
		if err := d.checkInstanceProfilePassRole(r.Context(), inv, accountID, in); err != nil {
			return err
		}
	}

	xmlOutput, err := h.dispatch(name, input, d, accountID)
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(xmlOutput); err != nil {
		slog.ErrorContext(r.Context(), "Failed to write IAM response", "err", err)
	}
	return nil
}

// getUserForCaller applies AWS's default for GetUser without UserName before
// authorization: a user is treated as naming themselves, root is described as
// the account root, and a role session must name a user.
func getUserForCaller(ctx context.Context, inv dispatch.Invocation, accountID string, in *iam.GetUserInput) (action, error) {
	// The actor is populated even when no caller ARN can be composed, and
	// GetUser needs only its principal type and name.
	actor, _ := inv.Actor()
	switch actor.PrincipalType {
	case principalTypeUser:
		if actor.UserName == "root" && accountID == awsidentifiers.GlobalAccountID {
			return getRootUser, nil
		}
		in.UserName = aws.String(actor.UserName)
		return actions["GetUser"], nil
	case principalTypeRoot:
		return getRootUser, nil
	case principalTypeAssumedRole:
		return action{}, awserrors.Errorf(awserrors.ErrorValidationError,
			"Must specify userName when calling with non-User credentials")
	default:
		slog.ErrorContext(ctx, "IAM GetUser: unknown principal type, cannot default UserName",
			"principal_type", actor.PrincipalType)
		return action{}, errors.New(awserrors.ErrorInternalError)
	}
}

// checkInstanceProfilePassRole requires iam:PassRole on the role being added to
// an instance profile: once added, every instance holding the profile receives
// its credentials, and the EC2 association check never sees the role.
func (d Deps) checkInstanceProfilePassRole(ctx context.Context, inv dispatch.Invocation, accountID string, input *iam.AddRoleToInstanceProfileInput) error {
	roleName := aws.StringValue(input.RoleName)
	if roleName == "" {
		return nil
	}
	roleARN, err := d.IAM.CanonicalResourceARN(accountID, arn.IAMRole, roleName)
	switch {
	case awserrors.IsErrorCode(err, awserrors.ErrorIAMNoSuchEntity):
		// Still evaluated, so a caller who cannot pass the role cannot use
		// the handler's NoSuchEntity to probe which roles exist.
		roleARN = arn.FormatIAMPath(arn.IAMRole, accountID, "/", roleName)
	case err != nil:
		slog.ErrorContext(ctx, "IAM: role lookup for iam:PassRole failed, failing closed",
			"account_id", accountID, "role_name", roleName, "err", err)
		return errors.New(awserrors.ErrorInternalError)
	}
	return inv.Authorize("iam", "PassRole", []string{roleARN},
		iampolicy.ConditionKeys{iampolicy.KeyPassedToService: ec2ServicePrincipal})
}
