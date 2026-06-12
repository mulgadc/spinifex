package gateway

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_tagging "github.com/mulgadc/spinifex/spinifex/gateway/tagging"
)

// taggingHandler invokes a per-action Resource Groups Tagging API function.
type taggingHandler func(gw *GatewayConfig, accountID string, body []byte) (any, error)

// taggingActions maps the action suffix of X-Amz-Target to its handler.
// The action is carried in "X-Amz-Target: ResourceGroupsTaggingAPI_20170126.<Action>".
var taggingActions = map[string]taggingHandler{
	"GetResources": func(gw *GatewayConfig, acct string, b []byte) (any, error) {
		return gateway_tagging.GetResources(gw.NATSConn, gw.Region, acct, b)
	},
}

// taggingActionFromTarget extracts the action suffix from an X-Amz-Target header.
func taggingActionFromTarget(target string) string {
	if i := strings.LastIndex(target, "."); i >= 0 {
		return target[i+1:]
	}
	return target
}

// Tagging_Request dispatches AWS JSON 1.1 Resource Groups Tagging API requests.
// The action comes from X-Amz-Target; errors are returned as awserrors codes.
func (gw *GatewayConfig) Tagging_Request(w http.ResponseWriter, r *http.Request) error {
	action := taggingActionFromTarget(r.Header.Get("X-Amz-Target"))
	if action == "" {
		return errors.New(awserrors.ErrorMissingAction)
	}

	handler, ok := taggingActions[action]
	if !ok {
		slog.Debug("tagging: unknown action", "action", action)
		return errors.New(awserrors.ErrorInvalidAction)
	}

	if err := gw.checkPolicy(r, "tagging", action); err != nil {
		return err
	}

	if gw.NATSConn == nil {
		return errors.New(awserrors.ErrorServerInternal)
	}

	accountID, _ := r.Context().Value(ctxAccountID).(string)
	if accountID == "" {
		slog.Error("Tagging_Request: no account ID in auth context")
		return errors.New(awserrors.ErrorServerInternal)
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("Tagging_Request: failed to read body", "err", err)
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	output, err := handler(gw, accountID, body)
	if err != nil {
		return err
	}

	gateway_tagging.WriteJSONResponse(w, output)
	return nil
}
