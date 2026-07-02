package gateway

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_ecs "github.com/mulgadc/spinifex/spinifex/gateway/ecs"
)

// ecsActionFromTarget extracts the action suffix from an X-Amz-Target header.
// Any "<Prefix>.<Action>" or bare "<Action>" form is accepted.
func ecsActionFromTarget(target string) string {
	if i := strings.LastIndex(target, "."); i >= 0 {
		return target[i+1:]
	}
	return target
}

// ECS_Request dispatches AWS JSON 1.1 ECS control-plane requests. The action
// comes from X-Amz-Target; errors are returned as awserrors codes and rendered
// by the shared ErrorHandler. Every action is a NotImplemented (501) stub until
// its real handler lands in a later Phase 4 sprint.
func (gw *GatewayConfig) ECS_Request(w http.ResponseWriter, r *http.Request) error {
	action := ecsActionFromTarget(r.Header.Get("X-Amz-Target"))
	if action == "" {
		return errors.New(awserrors.ErrorMissingAction)
	}

	handler, ok := gateway_ecs.Actions[action]
	if !ok {
		slog.Debug("ECS: unknown action", "action", action)
		return errors.New(awserrors.ErrorInvalidAction)
	}

	if err := gw.checkPolicy(r, "ecs", action); err != nil {
		return err
	}

	accountID, _ := r.Context().Value(ctxAccountID).(string)
	if accountID == "" {
		slog.Error("ECS_Request: no account ID in auth context")
		return errors.New(awserrors.ErrorServerInternal)
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("ECS_Request: failed to read body", "err", err)
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	output, err := handler(gw.NATSConn, accountID, body)
	if err != nil {
		return err
	}

	if gateway_ecs.RawJSONActions[action] {
		gateway_ecs.WriteRawJSONResponse(w, output)
		return nil
	}
	gateway_ecs.WriteJSONResponse(w, output)
	return nil
}
