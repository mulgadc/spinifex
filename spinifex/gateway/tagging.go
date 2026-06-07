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

// taggingHandler invokes a per-action Resource Groups Tagging API function: it
// receives the parent gateway config, the caller's accountID, and the raw JSON
// request body, and returns a typed *resourcegroupstaggingapi.<X>Output (as
// any) or an error whose Error() is an awserrors code.
type taggingHandler func(gw *GatewayConfig, accountID string, body []byte) (any, error)

// taggingActions maps a tagging action (the suffix of the X-Amz-Target header)
// to its handler. The Resource Groups Tagging API speaks AWS JSON 1.1: the
// action is carried in "X-Amz-Target: ResourceGroupsTaggingAPI_20170126.<Action>".
var taggingActions = map[string]taggingHandler{
	"GetResources": func(gw *GatewayConfig, acct string, b []byte) (any, error) {
		return gateway_tagging.GetResources(gw.NATSConn, gw.Region, acct, b)
	},
}

// taggingActionFromTarget extracts the bare action name from an X-Amz-Target
// header value of the form "ResourceGroupsTaggingAPI_20170126.<Action>".
func taggingActionFromTarget(target string) string {
	if i := strings.LastIndex(target, "."); i >= 0 {
		return target[i+1:]
	}
	return target
}

// Tagging_Request dispatches AWS JSON 1.1 Resource Groups Tagging API requests.
// The action is read from the X-Amz-Target header; the body is the JSON-encoded
// operation input. Errors are returned as plain awserrors codes; the caller
// (gateway.Request) routes them through ErrorHandler, which emits the JSON
// error envelope.
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
