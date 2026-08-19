package gateway

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/mulgadc/bluebottle/pkg/auth"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_rds "github.com/mulgadc/spinifex/spinifex/gateway/rds"
)

// RDS shares the query-in/XML-out shape of ELBv2: the action comes from the
// Action= form param and the response is the IAM-style XML envelope.
func (gw *GatewayConfig) RDS_Request(w http.ResponseWriter, r *http.Request) error {
	queryArgs, err := readQueryArgs(r)
	if err != nil {
		slog.DebugContext(r.Context(), "RDS: malformed query string", "err", err)
		return errors.New(awserrors.ErrorMalformedQueryString)
	}

	action := queryArgs["Action"]
	if action == "" {
		return errors.New(awserrors.ErrorMissingAction)
	}
	// Resolved before the policy check, so an unrecognised action is rejected as
	// InvalidAction rather than evaluated as an rds:<garbage> permission.
	if !gateway_rds.HasAction(action) {
		slog.DebugContext(r.Context(), "RDS: unknown action", "action", action)
		return errors.New(awserrors.ErrorInvalidAction)
	}

	caller, err := rdsCaller(r)
	if err != nil {
		slog.ErrorContext(r.Context(), "RDS_Request: no account ID in auth context")
		return err
	}

	// Before the policy check, so no customer grant is ever evaluated against an
	// internal agent action. See AuthorizeCaller.
	if err := gateway_rds.AuthorizeCaller(r.Context(), action, caller); err != nil {
		return err
	}

	resources, err := gateway_rds.ResourceARN(action, gw.Region, caller.AccountID, queryArgs)
	if err != nil {
		return err
	}
	if err := gw.checkPolicyResources(r, "rds", action, resources); err != nil {
		return err
	}

	if gw.NATSConn == nil {
		return errors.New(awserrors.ErrorServerInternal)
	}

	xmlOutput, err := gateway_rds.Dispatch(r.Context(), action, queryArgs, gw.NATSConn, caller,
		gateway_rds.Env{ExpectedNodes: gw.ExpectedNodes})
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(xmlOutput); err != nil {
		slog.ErrorContext(r.Context(), "Failed to write RDS response", "err", err)
	}
	return nil
}

// The role name comes from the underlying role ARN, never the session name,
// which the caller picks at AssumeRole time and could name its way past the gate.
func rdsCaller(r *http.Request) (gateway_rds.Caller, error) {
	accountID := mustCtxString(r, ctxAccountID)
	if accountID == "" {
		return gateway_rds.Caller{}, errors.New(awserrors.ErrorServerInternal)
	}
	caller := gateway_rds.Caller{
		AccountID:     accountID,
		PrincipalType: mustCtxString(r, ctxPrincipalType),
		SessionName:   mustCtxString(r, ctxIdentity),
	}
	if arn := mustCtxString(r, ctxUnderlyingRoleARN); arn != "" {
		if roleAcct, roleName, err := auth.ParseRoleARN(arn); err == nil && roleAcct == accountID {
			caller.RoleName = roleName
		}
	}
	return caller, nil
}
