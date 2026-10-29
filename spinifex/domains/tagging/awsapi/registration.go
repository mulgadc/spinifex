// Package awsapi registers the Resource Groups Tagging API with AWS ingress.
package awsapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/mulgadc/bluebottle/pkg/sigv4"
	"github.com/mulgadc/spinifex/spinifex/domains/tagging"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

// ServiceName is the SigV4 credential-scope service Tagging clients sign for.
const ServiceName = "tagging"

// Handler is the signature every Tagging action implements. accountID is the
// resolved caller account; body is the raw JSON 1.1 request payload.
type Handler func(ctx context.Context, agg *tagging.Aggregator, accountID string, body []byte) (any, error)

// Deps are the composed capabilities the Tagging control plane dispatches to.
// A nil Aggregator answers every action with ServerInternal.
type Deps struct {
	Aggregator *tagging.Aggregator
}

// Actions maps the Tagging action namespace onto the domain aggregator.
var Actions = map[string]Handler{
	"GetResources": func(ctx context.Context, agg *tagging.Aggregator, acct string, b []byte) (any, error) {
		return agg.GetResources(ctx, acct, b)
	},
}

// NewRegistration returns the Tagging AWS JSON 1.1 control-plane registration.
func NewRegistration(deps Deps) dispatch.Registration {
	return dispatch.Registration{
		Service:       ServiceName,
		ResolveAction: resolveAction,
		Dispatch:      deps.dispatch,
		Errors:        dispatch.ErrorEnvelopeJSON,
		Inventory:     OperationInventory(),
	}
}

// OperationInventory classifies the actions the registration serves. Every
// Tagging action is implemented, so none is Stubbed or Unsupported.
func OperationInventory() dispatch.Inventory {
	return dispatch.Inventory{
		Registered: slices.Sorted(maps.Keys(Actions)),
	}
}

// resolveAction is the one X-Amz-Target parser telemetry and dispatch share.
func resolveAction(r *http.Request) string {
	return actionFromTarget(r.Header.Get("X-Amz-Target"))
}

// actionFromTarget extracts the action suffix from an X-Amz-Target header.
// Any "<Prefix>.<Action>" or bare "<Action>" form is accepted.
func actionFromTarget(target string) string {
	if _, after, ok := strings.CutLast(target, "."); ok {
		return after
	}
	return target
}

// dispatch resolves, authorizes and runs one request. Authorization precedes
// the body read, so a denied caller is never told about a bad body. Errors are
// awserrors codes the gateway renders.
func (d Deps) dispatch(w http.ResponseWriter, inv dispatch.Invocation) error {
	r := inv.Request
	action := resolveAction(r)
	if action == "" {
		return errors.New(awserrors.ErrorMissingAction)
	}

	handler, ok := Actions[action]
	if !ok {
		slog.DebugContext(r.Context(), "tagging: unknown action", "action", action)
		return errors.New(awserrors.ErrorInvalidAction)
	}

	resources, err := tagging.ResourceARNs(action)
	if err != nil {
		return err
	}
	if err := inv.Authorize(ServiceName, action, resources, nil); err != nil {
		return err
	}

	if inv.AccountID == "" {
		slog.ErrorContext(r.Context(), "tagging dispatch: no account ID in auth context")
		return errors.New(awserrors.ErrorServerInternal)
	}

	body, err := readBoundedBody(r)
	if err != nil {
		slog.ErrorContext(r.Context(), "tagging dispatch: failed to read body", "err", err)
		return err
	}

	if d.Aggregator == nil {
		return errors.New(awserrors.ErrorServerInternal)
	}

	output, err := handler(r.Context(), d.Aggregator, inv.AccountID, body)
	if err != nil {
		return err
	}

	tagging.WriteJSONResponse(w, output)
	return nil
}

// readBoundedBody reads the body under the SigV4 payload cap, so a body can
// neither bypass it nor exhaust memory.
func readBoundedBody(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, sigv4.MaxPayloadLen+1))
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if int64(len(body)) > sigv4.MaxPayloadLen {
		return nil, errors.New(awserrors.ErrorRequestEntityTooLarge)
	}
	return body, nil
}
