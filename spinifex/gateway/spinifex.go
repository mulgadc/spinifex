package gateway

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_spx "github.com/mulgadc/spinifex/spinifex/gateway/spx"
)

// spinifexAdminActions lists actions that require admin account access.
var spinifexAdminActions = map[string]bool{
	"GetVersion":       true,
	"GetNodes":         true,
	"GetVMs":           true,
	"GetStorageStatus": true,
}

func (gw *GatewayConfig) Spinifex_Request(w http.ResponseWriter, r *http.Request) error {
	queryArgs, err := readQueryArgs(r)
	if err != nil {
		slog.Debug("Spinifex: malformed query string", "err", err)
		return errors.New(awserrors.ErrorMalformedQueryString)
	}

	action := queryArgs["Action"]
	if action == "" {
		// Also check query string for GET-style requests
		action = r.URL.Query().Get("Action")
	}
	if action == "" {
		return errors.New(awserrors.ErrorMissingAction)
	}

	if err := gw.checkPolicy(r, "spinifex", action); err != nil {
		return err
	}

	// Extract identity from auth context
	accountID, _ := r.Context().Value(ctxAccountID).(string)
	if accountID == "" {
		slog.Error("Spinifex_Request: no account ID in auth context")
		return errors.New(awserrors.ErrorServerInternal)
	}

	// Enforce admin-only on restricted actions
	if spinifexAdminActions[action] && accountID != admin.DefaultAccountID() {
		slog.Info("Spinifex_Request: non-admin access denied", "action", action, "accountID", accountID)
		return errors.New(awserrors.ErrorAccessDenied)
	}

	var output any
	switch action {
	case "GetVersion":
		output, err = gateway_spx.GetVersion(gw.Version, gw.Commit)
	case "GetNodes":
		if gw.NATSConn == nil {
			return errors.New(awserrors.ErrorServerInternal)
		}
		output, err = gateway_spx.GetNodes(gw.NATSConn, gw.DiscoverActiveNodes())
	case "GetVMs":
		if gw.NATSConn == nil {
			return errors.New(awserrors.ErrorServerInternal)
		}
		output, err = gateway_spx.GetVMs(gw.NATSConn, gw.DiscoverActiveNodes())
	case "GetStorageStatus":
		if gw.NATSConn == nil {
			return errors.New(awserrors.ErrorServerInternal)
		}
		output, err = gateway_spx.GetStorageStatus(gw.NATSConn)
	default:
		return errors.New(awserrors.ErrorInvalidAction)
	}

	if err != nil {
		return err
	}

	jsonOutput, err := json.Marshal(output)
	if err != nil {
		slog.Error("Failed to marshal spinifex response", "action", action, "err", err)
		return errors.New(awserrors.ErrorInternalError)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(jsonOutput); err != nil {
		slog.Error("Failed to write spinifex response", "err", err)
	}
	return nil
}
