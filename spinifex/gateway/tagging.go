package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/domains/tagging"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// taggingHandler invokes a per-action Resource Groups Tagging API function.
type taggingHandler func(ctx context.Context, gw *GatewayConfig, accountID string, body []byte) (any, error)

// taggingActions maps the action suffix of X-Amz-Target to its handler.
// The action is carried in "X-Amz-Target: ResourceGroupsTaggingAPI_20170126.<Action>".
var taggingActions = map[string]taggingHandler{
	"GetResources": func(ctx context.Context, gw *GatewayConfig, acct string, b []byte) (any, error) {
		return gw.TaggingResources.GetResources(ctx, acct, b)
	},
}

// taggingActionFromTarget extracts the action suffix from an X-Amz-Target header.
func taggingActionFromTarget(target string) string {
	if _, after, ok := strings.CutLast(target, "."); ok {
		return after
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

	resources, err := tagging.ResourceARNs(action)
	if err != nil {
		return err
	}
	if err := gw.checkPolicyResources(r, "tagging", action, resources); err != nil {
		return err
	}

	accountID, _ := r.Context().Value(ctxAccountID).(string)
	if accountID == "" {
		slog.Error("Tagging_Request: no account ID in auth context")
		return errors.New(awserrors.ErrorServerInternal)
	}

	// Read ahead of the NATS guard so an oversized body is rejected on its own
	// terms rather than behind a connection that happens to be down.
	body, err := readBoundedBody(r)
	if err != nil {
		slog.Error("Tagging_Request: failed to read body", "err", err)
		return err
	}

	if gw.NATSConn == nil || gw.TaggingResources == nil {
		return errors.New(awserrors.ErrorServerInternal)
	}

	output, err := handler(r.Context(), gw, accountID, body)
	if err != nil {
		return err
	}

	tagging.WriteJSONResponse(w, output)
	return nil
}
