package gateway

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awsec2query"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_ec2_instance "github.com/mulgadc/spinifex/spinifex/gateway/ec2/instance"
	gateway_iam "github.com/mulgadc/spinifex/spinifex/gateway/iam"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// IAMHandler processes parsed query args and returns XML response bytes.
type IAMHandler func(action string, q map[string]string, gw *GatewayConfig, accountID string) ([]byte, error)

// iamHandler creates a type-safe IAMHandler: allocates the input struct,
// parses query params, calls the handler, and marshals output to XML.
func iamHandler[In any](handler func(string, *In, *GatewayConfig) (any, error)) IAMHandler {
	return func(action string, q map[string]string, gw *GatewayConfig, accountID string) ([]byte, error) {
		input := new(In)
		if err := awsec2query.QueryParamsToStruct(q, input); err != nil {
			if errors.Is(err, awsec2query.ErrSliceTooLarge) {
				return nil, errors.New(awserrors.ErrorMalformedQueryString)
			}
			return nil, errors.New(awserrors.ErrorIAMInvalidInput)
		}
		output, err := handler(accountID, input, gw)
		if err != nil {
			return nil, err
		}
		payload := utils.GenerateIAMXMLPayload(action, output)
		xmlOutput, err := utils.MarshalToXML(payload)
		if err != nil {
			return nil, errors.New(awserrors.ErrorInternalError)
		}
		return xmlOutput, nil
	}
}

var iamActions = map[string]IAMHandler{
	"CreateUser": iamHandler(func(accountID string, input *iam.CreateUserInput, gw *GatewayConfig) (any, error) {
		return gateway_iam.CreateUser(accountID, input, gw.IAMService)
	}),
	"GetUser": iamHandler(func(accountID string, input *iam.GetUserInput, gw *GatewayConfig) (any, error) {
		return gateway_iam.GetUser(accountID, input, gw.IAMService)
	}),
	"ListUsers": iamHandler(func(accountID string, input *iam.ListUsersInput, gw *GatewayConfig) (any, error) {
		return gateway_iam.ListUsers(accountID, input, gw.IAMService)
	}),
	"DeleteUser": iamHandler(func(accountID string, input *iam.DeleteUserInput, gw *GatewayConfig) (any, error) {
		return gateway_iam.DeleteUser(accountID, input, gw.IAMService)
	}),
	"CreateAccessKey": iamHandler(func(accountID string, input *iam.CreateAccessKeyInput, gw *GatewayConfig) (any, error) {
		return gateway_iam.CreateAccessKey(accountID, input, gw.IAMService)
	}),
	"ListAccessKeys": iamHandler(func(accountID string, input *iam.ListAccessKeysInput, gw *GatewayConfig) (any, error) {
		return gateway_iam.ListAccessKeys(accountID, input, gw.IAMService)
	}),
	"DeleteAccessKey": iamHandler(func(accountID string, input *iam.DeleteAccessKeyInput, gw *GatewayConfig) (any, error) {
		return gateway_iam.DeleteAccessKey(accountID, input, gw.IAMService)
	}),
	"UpdateAccessKey": iamHandler(func(accountID string, input *iam.UpdateAccessKeyInput, gw *GatewayConfig) (any, error) {
		return gateway_iam.UpdateAccessKey(accountID, input, gw.IAMService)
	}),

	// Policy CRUD
	"CreatePolicy": iamHandler(func(accountID string, input *iam.CreatePolicyInput, gw *GatewayConfig) (any, error) {
		return gateway_iam.CreatePolicy(accountID, input, gw.IAMService)
	}),
	"GetPolicy": iamHandler(func(accountID string, input *iam.GetPolicyInput, gw *GatewayConfig) (any, error) {
		return gateway_iam.GetPolicy(accountID, input, gw.IAMService)
	}),
	"GetPolicyVersion": iamHandler(func(accountID string, input *iam.GetPolicyVersionInput, gw *GatewayConfig) (any, error) {
		return gateway_iam.GetPolicyVersion(accountID, input, gw.IAMService)
	}),
	"ListPolicyVersions": iamHandler(func(accountID string, input *iam.ListPolicyVersionsInput, gw *GatewayConfig) (any, error) {
		return gateway_iam.ListPolicyVersions(accountID, input, gw.IAMService)
	}),
	"ListPolicies": iamHandler(func(accountID string, input *iam.ListPoliciesInput, gw *GatewayConfig) (any, error) {
		return gateway_iam.ListPolicies(accountID, input, gw.IAMService)
	}),
	"DeletePolicy": iamHandler(func(accountID string, input *iam.DeletePolicyInput, gw *GatewayConfig) (any, error) {
		return gateway_iam.DeletePolicy(accountID, input, gw.IAMService)
	}),

	// Policy attachment
	"AttachUserPolicy": iamHandler(func(accountID string, input *iam.AttachUserPolicyInput, gw *GatewayConfig) (any, error) {
		return gateway_iam.AttachUserPolicy(accountID, input, gw.IAMService)
	}),
	"DetachUserPolicy": iamHandler(func(accountID string, input *iam.DetachUserPolicyInput, gw *GatewayConfig) (any, error) {
		return gateway_iam.DetachUserPolicy(accountID, input, gw.IAMService)
	}),
	"ListAttachedUserPolicies": iamHandler(func(accountID string, input *iam.ListAttachedUserPoliciesInput, gw *GatewayConfig) (any, error) {
		return gateway_iam.ListAttachedUserPolicies(accountID, input, gw.IAMService)
	}),

	// Role CRUD
	"CreateRole": iamHandler(func(accountID string, input *iam.CreateRoleInput, gw *GatewayConfig) (any, error) {
		return gateway_iam.CreateRole(accountID, input, gw.IAMService)
	}),
	"GetRole": iamHandler(func(accountID string, input *iam.GetRoleInput, gw *GatewayConfig) (any, error) {
		return gateway_iam.GetRole(accountID, input, gw.IAMService)
	}),
	"ListRoles": iamHandler(func(accountID string, input *iam.ListRolesInput, gw *GatewayConfig) (any, error) {
		return gateway_iam.ListRoles(accountID, input, gw.IAMService)
	}),
	"DeleteRole": iamHandler(func(accountID string, input *iam.DeleteRoleInput, gw *GatewayConfig) (any, error) {
		return gateway_iam.DeleteRole(accountID, input, gw.IAMService)
	}),
	"UpdateRole": iamHandler(func(accountID string, input *iam.UpdateRoleInput, gw *GatewayConfig) (any, error) {
		return gateway_iam.UpdateRole(accountID, input, gw.IAMService)
	}),
	"UpdateAssumeRolePolicy": iamHandler(func(accountID string, input *iam.UpdateAssumeRolePolicyInput, gw *GatewayConfig) (any, error) {
		return gateway_iam.UpdateAssumeRolePolicy(accountID, input, gw.IAMService)
	}),

	// Role policy attachment
	"AttachRolePolicy": iamHandler(func(accountID string, input *iam.AttachRolePolicyInput, gw *GatewayConfig) (any, error) {
		return gateway_iam.AttachRolePolicy(accountID, input, gw.IAMService)
	}),
	"DetachRolePolicy": iamHandler(func(accountID string, input *iam.DetachRolePolicyInput, gw *GatewayConfig) (any, error) {
		return gateway_iam.DetachRolePolicy(accountID, input, gw.IAMService)
	}),
	"ListAttachedRolePolicies": iamHandler(func(accountID string, input *iam.ListAttachedRolePoliciesInput, gw *GatewayConfig) (any, error) {
		return gateway_iam.ListAttachedRolePolicies(accountID, input, gw.IAMService)
	}),
	"ListRolePolicies": iamHandler(func(accountID string, input *iam.ListRolePoliciesInput, gw *GatewayConfig) (any, error) {
		return gateway_iam.ListRolePolicies(accountID, input, gw.IAMService)
	}),

	// Instance profile CRUD
	"CreateInstanceProfile": iamHandler(func(accountID string, input *iam.CreateInstanceProfileInput, gw *GatewayConfig) (any, error) {
		return gateway_iam.CreateInstanceProfile(accountID, input, gw.IAMService)
	}),
	"GetInstanceProfile": iamHandler(func(accountID string, input *iam.GetInstanceProfileInput, gw *GatewayConfig) (any, error) {
		return gateway_iam.GetInstanceProfile(accountID, input, gw.IAMService)
	}),
	"ListInstanceProfiles": iamHandler(func(accountID string, input *iam.ListInstanceProfilesInput, gw *GatewayConfig) (any, error) {
		return gateway_iam.ListInstanceProfiles(accountID, input, gw.IAMService)
	}),
	"DeleteInstanceProfile": iamHandler(func(accountID string, input *iam.DeleteInstanceProfileInput, gw *GatewayConfig) (any, error) {
		countLive := func(profileARN string) (int, error) {
			return gateway_ec2_instance.CountInstanceProfileAssociations(gw.NATSConn, gw.DiscoverActiveNodes(), accountID, profileARN)
		}
		return gateway_iam.DeleteInstanceProfile(accountID, input, gw.IAMService, countLive)
	}),
	"ListInstanceProfilesForRole": iamHandler(func(accountID string, input *iam.ListInstanceProfilesForRoleInput, gw *GatewayConfig) (any, error) {
		return gateway_iam.ListInstanceProfilesForRole(accountID, input, gw.IAMService)
	}),

	// Instance profile ↔ role binding
	"AddRoleToInstanceProfile": iamHandler(func(accountID string, input *iam.AddRoleToInstanceProfileInput, gw *GatewayConfig) (any, error) {
		return gateway_iam.AddRoleToInstanceProfile(accountID, input, gw.IAMService)
	}),
	"RemoveRoleFromInstanceProfile": iamHandler(func(accountID string, input *iam.RemoveRoleFromInstanceProfileInput, gw *GatewayConfig) (any, error) {
		return gateway_iam.RemoveRoleFromInstanceProfile(accountID, input, gw.IAMService)
	}),

	// OIDC identity-provider registry (IRSA)
	"CreateOpenIDConnectProvider": iamHandler(func(accountID string, input *iam.CreateOpenIDConnectProviderInput, gw *GatewayConfig) (any, error) {
		return gateway_iam.CreateOpenIDConnectProvider(accountID, input, gw.IAMService)
	}),
	"GetOpenIDConnectProvider": iamHandler(func(accountID string, input *iam.GetOpenIDConnectProviderInput, gw *GatewayConfig) (any, error) {
		return gateway_iam.GetOpenIDConnectProvider(accountID, input, gw.IAMService)
	}),
	"ListOpenIDConnectProviders": iamHandler(func(accountID string, input *iam.ListOpenIDConnectProvidersInput, gw *GatewayConfig) (any, error) {
		return gateway_iam.ListOpenIDConnectProviders(accountID, input, gw.IAMService)
	}),
	"DeleteOpenIDConnectProvider": iamHandler(func(accountID string, input *iam.DeleteOpenIDConnectProviderInput, gw *GatewayConfig) (any, error) {
		return gateway_iam.DeleteOpenIDConnectProvider(accountID, input, gw.IAMService)
	}),
}

func (gw *GatewayConfig) IAM_Request(w http.ResponseWriter, r *http.Request) error {
	queryArgs, err := readQueryArgs(r)
	if err != nil {
		slog.Debug("IAM: malformed query string", "err", err)
		return errors.New(awserrors.ErrorMalformedQueryString)
	}

	action := queryArgs["Action"]
	if action == "" {
		return errors.New(awserrors.ErrorMissingAction)
	}
	handler, ok := iamActions[action]
	if !ok {
		slog.Debug("IAM: unknown action", "action", action)
		return errors.New(awserrors.ErrorInvalidAction)
	}

	if gw.IAMService == nil {
		slog.Error("IAM: service not initialized")
		return errors.New(awserrors.ErrorInternalError)
	}

	if err := gw.checkPolicy(r, "iam", action); err != nil {
		return err
	}

	accountID, _ := r.Context().Value(ctxAccountID).(string)
	if accountID == "" {
		slog.Error("IAM_Request: no account ID in auth context")
		return errors.New(awserrors.ErrorInternalError)
	}

	xmlOutput, err := handler(action, queryArgs, gw, accountID)
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(xmlOutput); err != nil {
		slog.Error("Failed to write IAM response", "err", err)
	}
	return nil
}
