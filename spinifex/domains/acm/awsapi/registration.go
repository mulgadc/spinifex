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
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/nats-io/nats.go"
)

// ServiceName is the SigV4 credential-scope service ACM clients sign for.
const ServiceName = "acm"

// Handler is the signature every ACM control-plane action implements. nc is
// the gateway NATS connection (handlers relay onto acm.* subjects); accountID
// is the resolved caller account; body is the raw JSON 1.1 request payload.
type Handler func(ctx context.Context, nc *nats.Conn, accountID string, body []byte) (any, error)

// Deps are the composed capabilities the ACM control plane dispatches to. A
// nil NATS connection answers every action with ServerInternal.
type Deps struct {
	NATS *nats.Conn
}

// Actions maps the ACM control-plane action namespace onto the domain's
// existing NATS-backed adapters.
var Actions = map[string]Handler{
	"ImportCertificate": func(ctx context.Context, nc *nats.Conn, acct string, b []byte) (any, error) {
		return ImportCertificate(ctx, nc, acct, b)
	},
	"RequestCertificate": func(ctx context.Context, nc *nats.Conn, acct string, b []byte) (any, error) {
		return RequestCertificate(ctx, nc, acct, b)
	},
	"DescribeCertificate": func(ctx context.Context, nc *nats.Conn, acct string, b []byte) (any, error) {
		return DescribeCertificate(ctx, nc, acct, b)
	},
	"GetCertificate": func(ctx context.Context, nc *nats.Conn, acct string, b []byte) (any, error) {
		return GetCertificate(ctx, nc, acct, b)
	},
	"ListCertificates": func(ctx context.Context, nc *nats.Conn, acct string, b []byte) (any, error) {
		return ListCertificates(ctx, nc, acct, b)
	},
	"DeleteCertificate": func(ctx context.Context, nc *nats.Conn, acct string, b []byte) (any, error) {
		return DeleteCertificate(ctx, nc, acct, b)
	},
	"ListTagsForCertificate": func(ctx context.Context, nc *nats.Conn, acct string, b []byte) (any, error) {
		return ListTagsForCertificate(ctx, nc, acct, b)
	},
	"AddTagsToCertificate": func(ctx context.Context, nc *nats.Conn, acct string, b []byte) (any, error) {
		return AddTagsToCertificate(ctx, nc, acct, b)
	},
	"RemoveTagsFromCertificate": func(ctx context.Context, nc *nats.Conn, acct string, b []byte) (any, error) {
		return RemoveTagsFromCertificate(ctx, nc, acct, b)
	},
}

// NewRegistration returns the ACM AWS JSON 1.1 control-plane registration.
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
// ACM action is implemented, so none is Stubbed or Unsupported.
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
	if i := strings.LastIndex(target, "."); i >= 0 {
		return target[i+1:]
	}
	return target
}

// dispatch resolves, scopes and authorizes the request from one read of its
// body, then runs the action. Errors are awserrors codes the gateway renders.
func (d Deps) dispatch(w http.ResponseWriter, inv dispatch.Invocation) error {
	r := inv.Request
	action := resolveAction(r)
	if action == "" {
		return errors.New(awserrors.ErrorMissingAction)
	}

	handler, ok := Actions[action]
	if !ok {
		slog.DebugContext(r.Context(), "ACM: unknown action", "action", action)
		return errors.New(awserrors.ErrorInvalidAction)
	}

	accountID := inv.AccountID
	if accountID == "" {
		slog.ErrorContext(r.Context(), "ACM dispatch: no account ID in auth context")
		// InternalError, not ServerInternal: the policy gate used to reach this
		// case first and that is the code the caller has always seen.
		return errors.New(awserrors.ErrorInternalError)
	}

	body, err := readBoundedBody(r)
	if err != nil {
		slog.ErrorContext(r.Context(), "ACM dispatch: failed to read body", "err", err)
		return err
	}

	resources, err := ResourceARNs(action, inv.Region, accountID, body)
	if err != nil {
		return err
	}
	if err := inv.Authorize(ServiceName, action, resources, nil); err != nil {
		return err
	}

	if d.NATS == nil {
		return errors.New(awserrors.ErrorServerInternal)
	}

	output, err := handler(r.Context(), d.NATS, accountID, body)
	if err != nil {
		return err
	}

	WriteJSONResponse(w, output)
	return nil
}

// readBoundedBody reads the body under the SigV4 payload cap, so a body read
// ahead of the policy gate can neither bypass it nor exhaust memory.
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
