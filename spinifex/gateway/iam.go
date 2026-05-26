package gateway

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awsec2query"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_iam "github.com/mulgadc/spinifex/spinifex/gateway/iam"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// IAMHandler processes parsed query args and returns XML response bytes.
type IAMHandler func(action string, q map[string]string, gw *GatewayConfig, accountID string) ([]byte, error)

// iamHandler creates a type-safe IAMHandler that allocates the typed input struct,
// parses query params into it, calls the handler, and marshals the output to XML.
func iamHandler[In any](handler func(string, *In, handlers_iam.IAMService) (any, error)) IAMHandler {
	return func(action string, q map[string]string, gw *GatewayConfig, accountID string) ([]byte, error) {
		input := new(In)
		if err := awsec2query.QueryParamsToStruct(q, input); err != nil {
			if errors.Is(err, awsec2query.ErrSliceTooLarge) {
				return nil, errors.New(awserrors.ErrorMalformedQueryString)
			}
			return nil, errors.New(awserrors.ErrorIAMInvalidInput)
		}
		output, err := handler(accountID, input, gw.IAMService)
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
	"CreateUser": iamHandler(func(accountID string, input *iam.CreateUserInput, svc handlers_iam.IAMService) (any, error) {
		return gateway_iam.CreateUser(accountID, input, svc)
	}),
	"GetUser": iamHandler(func(accountID string, input *iam.GetUserInput, svc handlers_iam.IAMService) (any, error) {
		return gateway_iam.GetUser(accountID, input, svc)
	}),
	"ListUsers": iamHandler(func(accountID string, input *iam.ListUsersInput, svc handlers_iam.IAMService) (any, error) {
		return gateway_iam.ListUsers(accountID, input, svc)
	}),
	"DeleteUser": iamHandler(func(accountID string, input *iam.DeleteUserInput, svc handlers_iam.IAMService) (any, error) {
		return gateway_iam.DeleteUser(accountID, input, svc)
	}),
	"CreateAccessKey": iamHandler(func(accountID string, input *iam.CreateAccessKeyInput, svc handlers_iam.IAMService) (any, error) {
		return gateway_iam.CreateAccessKey(accountID, input, svc)
	}),
	"ListAccessKeys": iamHandler(func(accountID string, input *iam.ListAccessKeysInput, svc handlers_iam.IAMService) (any, error) {
		return gateway_iam.ListAccessKeys(accountID, input, svc)
	}),
	"DeleteAccessKey": iamHandler(func(accountID string, input *iam.DeleteAccessKeyInput, svc handlers_iam.IAMService) (any, error) {
		return gateway_iam.DeleteAccessKey(accountID, input, svc)
	}),
	"UpdateAccessKey": iamHandler(func(accountID string, input *iam.UpdateAccessKeyInput, svc handlers_iam.IAMService) (any, error) {
		return gateway_iam.UpdateAccessKey(accountID, input, svc)
	}),

	// Policy CRUD
	"CreatePolicy": iamHandler(func(accountID string, input *iam.CreatePolicyInput, svc handlers_iam.IAMService) (any, error) {
		return gateway_iam.CreatePolicy(accountID, input, svc)
	}),
	"GetPolicy": iamHandler(func(accountID string, input *iam.GetPolicyInput, svc handlers_iam.IAMService) (any, error) {
		return gateway_iam.GetPolicy(accountID, input, svc)
	}),
	"GetPolicyVersion": iamHandler(func(accountID string, input *iam.GetPolicyVersionInput, svc handlers_iam.IAMService) (any, error) {
		return gateway_iam.GetPolicyVersion(accountID, input, svc)
	}),
	"ListPolicies": iamHandler(func(accountID string, input *iam.ListPoliciesInput, svc handlers_iam.IAMService) (any, error) {
		return gateway_iam.ListPolicies(accountID, input, svc)
	}),
	"DeletePolicy": iamHandler(func(accountID string, input *iam.DeletePolicyInput, svc handlers_iam.IAMService) (any, error) {
		return gateway_iam.DeletePolicy(accountID, input, svc)
	}),

	// Policy attachment
	"AttachUserPolicy": iamHandler(func(accountID string, input *iam.AttachUserPolicyInput, svc handlers_iam.IAMService) (any, error) {
		return gateway_iam.AttachUserPolicy(accountID, input, svc)
	}),
	"DetachUserPolicy": iamHandler(func(accountID string, input *iam.DetachUserPolicyInput, svc handlers_iam.IAMService) (any, error) {
		return gateway_iam.DetachUserPolicy(accountID, input, svc)
	}),
	"ListAttachedUserPolicies": iamHandler(func(accountID string, input *iam.ListAttachedUserPoliciesInput, svc handlers_iam.IAMService) (any, error) {
		return gateway_iam.ListAttachedUserPolicies(accountID, input, svc)
	}),

	// Role CRUD
	"CreateRole": iamHandler(func(accountID string, input *iam.CreateRoleInput, svc handlers_iam.IAMService) (any, error) {
		return gateway_iam.CreateRole(accountID, input, svc)
	}),
	"GetRole": iamHandler(func(accountID string, input *iam.GetRoleInput, svc handlers_iam.IAMService) (any, error) {
		return gateway_iam.GetRole(accountID, input, svc)
	}),
	"ListRoles": iamHandler(func(accountID string, input *iam.ListRolesInput, svc handlers_iam.IAMService) (any, error) {
		return gateway_iam.ListRoles(accountID, input, svc)
	}),
	"DeleteRole": iamHandler(func(accountID string, input *iam.DeleteRoleInput, svc handlers_iam.IAMService) (any, error) {
		return gateway_iam.DeleteRole(accountID, input, svc)
	}),
	"UpdateRole": iamHandler(func(accountID string, input *iam.UpdateRoleInput, svc handlers_iam.IAMService) (any, error) {
		return gateway_iam.UpdateRole(accountID, input, svc)
	}),
	"UpdateAssumeRolePolicy": iamHandler(func(accountID string, input *iam.UpdateAssumeRolePolicyInput, svc handlers_iam.IAMService) (any, error) {
		return gateway_iam.UpdateAssumeRolePolicy(accountID, input, svc)
	}),

	// Role policy attachment
	"AttachRolePolicy": iamHandler(func(accountID string, input *iam.AttachRolePolicyInput, svc handlers_iam.IAMService) (any, error) {
		return gateway_iam.AttachRolePolicy(accountID, input, svc)
	}),
	"DetachRolePolicy": iamHandler(func(accountID string, input *iam.DetachRolePolicyInput, svc handlers_iam.IAMService) (any, error) {
		return gateway_iam.DetachRolePolicy(accountID, input, svc)
	}),
	"ListAttachedRolePolicies": iamHandler(func(accountID string, input *iam.ListAttachedRolePoliciesInput, svc handlers_iam.IAMService) (any, error) {
		return gateway_iam.ListAttachedRolePolicies(accountID, input, svc)
	}),

	// Instance profile CRUD
	"CreateInstanceProfile": iamHandler(func(accountID string, input *iam.CreateInstanceProfileInput, svc handlers_iam.IAMService) (any, error) {
		return gateway_iam.CreateInstanceProfile(accountID, input, svc)
	}),
	"GetInstanceProfile": iamHandler(func(accountID string, input *iam.GetInstanceProfileInput, svc handlers_iam.IAMService) (any, error) {
		return gateway_iam.GetInstanceProfile(accountID, input, svc)
	}),
	"ListInstanceProfiles": iamHandler(func(accountID string, input *iam.ListInstanceProfilesInput, svc handlers_iam.IAMService) (any, error) {
		return gateway_iam.ListInstanceProfiles(accountID, input, svc)
	}),
	"DeleteInstanceProfile": iamHandler(func(accountID string, input *iam.DeleteInstanceProfileInput, svc handlers_iam.IAMService) (any, error) {
		return gateway_iam.DeleteInstanceProfile(accountID, input, svc)
	}),
	"ListInstanceProfilesForRole": iamHandler(func(accountID string, input *iam.ListInstanceProfilesForRoleInput, svc handlers_iam.IAMService) (any, error) {
		return gateway_iam.ListInstanceProfilesForRole(accountID, input, svc)
	}),

	// Instance profile ↔ role binding
	"AddRoleToInstanceProfile": iamHandler(func(accountID string, input *iam.AddRoleToInstanceProfileInput, svc handlers_iam.IAMService) (any, error) {
		return gateway_iam.AddRoleToInstanceProfile(accountID, input, svc)
	}),
	"RemoveRoleFromInstanceProfile": iamHandler(func(accountID string, input *iam.RemoveRoleFromInstanceProfileInput, svc handlers_iam.IAMService) (any, error) {
		return gateway_iam.RemoveRoleFromInstanceProfile(accountID, input, svc)
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

	// Extract account ID from auth context
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
