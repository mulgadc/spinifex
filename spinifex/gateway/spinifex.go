package gateway

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_bedrock "github.com/mulgadc/spinifex/spinifex/gateway/bedrock"
	gateway_spx "github.com/mulgadc/spinifex/spinifex/gateway/spx"
)

// spinifexAdminActions lists actions that require admin account access.
var spinifexAdminActions = map[string]bool{
	"GetVersion":        true,
	"GetNodes":          true,
	"GetVMs":            true,
	"GetStorageStatus":  true,
	"PromoteImage":      true,
	"GrantModelAccess":  true,
	"RevokeModelAccess": true,
	"ListModelAccess":   true,
}

func (gw *GatewayConfig) Spinifex_Request(w http.ResponseWriter, r *http.Request) error {
	queryArgs, err := readQueryArgs(r)
	if err != nil {
		slog.Debug("Spinifex: malformed query string", "err", err)
		return errors.New(awserrors.ErrorMalformedQueryString)
	}

	action := queryArgs["Action"]
	if action == "" {
		action = r.URL.Query().Get("Action")
	}
	if action == "" {
		return errors.New(awserrors.ErrorMissingAction)
	}

	if err := gw.checkPolicy(r, "spinifex", action); err != nil {
		return err
	}

	accountID, _ := r.Context().Value(ctxAccountID).(string)
	if accountID == "" {
		slog.Error("Spinifex_Request: no account ID in auth context")
		return errors.New(awserrors.ErrorServerInternal)
	}

	if spinifexAdminActions[action] && accountID != admin.DefaultAccountID() {
		slog.Info("Spinifex_Request: non-admin access denied", "action", action, "accountID", accountID)
		return errors.New(awserrors.ErrorAccessDenied)
	}

	ctx := r.Context()
	var output any
	switch action {
	case "GetVersion":
		output, err = gateway_spx.GetVersion(gw.Version, gw.Commit)
	case "GetNodes":
		if gw.NATSConn == nil {
			return errors.New(awserrors.ErrorServerInternal)
		}
		output, err = gateway_spx.GetNodes(ctx, gw.NATSConn, gw.DiscoverActiveNodes(ctx))
	case "GetVMs":
		if gw.NATSConn == nil {
			return errors.New(awserrors.ErrorServerInternal)
		}
		output, err = gateway_spx.GetVMs(ctx, gw.NATSConn, gw.DiscoverActiveNodes(ctx))
	case "GetStorageStatus":
		if gw.NATSConn == nil {
			return errors.New(awserrors.ErrorServerInternal)
		}
		output, err = gateway_spx.GetStorageStatus(gw.NATSConn)
	case "PromoteImage":
		if gw.NATSConn == nil {
			return errors.New(awserrors.ErrorServerInternal)
		}
		imageID := queryArgs["ImageId"]
		if imageID == "" {
			return errors.New(awserrors.ErrorMissingParameter)
		}
		output, err = gateway_spx.PromoteImage(ctx, gw.NATSConn, imageID, accountID)
	case "GrantModelAccess", "RevokeModelAccess":
		// Grants are gateway-owned KV state, like the bedrock credential store,
		// so these need no NATS hop: the gateway writes the bucket it reads.
		if gw.BedrockAccessAdmin == nil {
			return errors.New(awserrors.ErrorServerInternal)
		}
		targetAccount, modelID := queryArgs["AccountId"], queryArgs["ModelId"]
		if targetAccount == "" || modelID == "" {
			return errors.New(awserrors.ErrorMissingParameter)
		}
		if action == "GrantModelAccess" {
			err = gw.BedrockAccessAdmin.Grant(ctx, targetAccount, modelID)
		} else {
			err = gw.BedrockAccessAdmin.Revoke(ctx, targetAccount, modelID)
		}
		output = gateway_bedrock.ModelAccessChange{AccountID: targetAccount, ModelID: modelID}
	case "ListModelAccess":
		if gw.BedrockAccessAdmin == nil {
			return errors.New(awserrors.ErrorServerInternal)
		}
		targetAccount := queryArgs["AccountId"]
		if targetAccount == "" {
			return errors.New(awserrors.ErrorMissingParameter)
		}
		var models []string
		if models, err = gw.BedrockAccessAdmin.List(ctx, targetAccount); err == nil {
			output = gateway_bedrock.ModelAccessList{AccountID: targetAccount, ModelIDs: models}
		}
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
