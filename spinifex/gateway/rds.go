package gateway

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_rds "github.com/mulgadc/spinifex/spinifex/gateway/rds"
)

// RDS_Request dispatches AWS Query-protocol RDS requests. RDS shares the
// query-in/XML-out shape of ELBv2 (rds-v1.md D2): the action comes from the
// Action= form param and the response is the IAM-style XML envelope, so the
// aws-sdk-go query unmarshaler reads both success and error bodies natively.
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
	// Resolve the action before the policy check so an unrecognised one is
	// rejected as InvalidAction rather than evaluated as an rds:<garbage>
	// permission the caller could never hold.
	if !gateway_rds.HasAction(action) {
		slog.DebugContext(r.Context(), "RDS: unknown action", "action", action)
		return errors.New(awserrors.ErrorInvalidAction)
	}

	if err := gw.checkPolicy(r, "rds", action); err != nil {
		return err
	}

	if gw.NATSConn == nil {
		return errors.New(awserrors.ErrorServerInternal)
	}

	accountID, _ := r.Context().Value(ctxAccountID).(string)
	if accountID == "" {
		slog.ErrorContext(r.Context(), "RDS_Request: no account ID in auth context")
		return errors.New(awserrors.ErrorServerInternal)
	}

	xmlOutput, err := gateway_rds.Dispatch(r.Context(), action, queryArgs, gw.NATSConn, accountID)
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
