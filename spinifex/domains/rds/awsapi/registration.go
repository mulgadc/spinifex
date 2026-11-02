package awsapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/mulgadc/bluebottle/pkg/sigv4"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/query"
	"github.com/nats-io/nats.go"
)

// ServiceName is the SigV4 credential-scope service RDS clients sign for.
const ServiceName = "rds"

// Quota is the account DB-instance cap a create consults.
type Quota interface {
	EnforceRDSInstances(ctx context.Context, nc *nats.Conn, accountID string, want int) error
}

// Deps are the composed capabilities the RDS control plane dispatches to. A
// nil NATS connection answers every action with ServerInternal and a nil Quota
// skips the DB-instance cap.
type Deps struct {
	NATS          *nats.Conn
	ExpectedNodes int
	Quota         Quota
}

// NewRegistration returns the RDS AWS Query control-plane registration.
func NewRegistration(deps Deps) dispatch.Registration {
	return dispatch.Registration{
		Service:       ServiceName,
		ResolveAction: resolveAction,
		Dispatch:      deps.dispatch,
		Errors:        dispatch.ErrorEnvelopeXML,
		Inventory:     OperationInventory(),
	}
}

// OperationInventory classifies the actions the registration serves. The
// recognised-but-unavailable ones are Unsupported, so they are not implemented.
func OperationInventory() dispatch.Inventory {
	return dispatch.Inventory{
		Registered:  ActionNames(),
		Unsupported: UnsupportedActionNames(),
	}
}

// resolveAction is the one Query Action parser telemetry and dispatch share.
// It leaves the body readable for the dispatcher and answers "" on any failure.
func resolveAction(r *http.Request) string {
	args, err := readQueryArgs(r)
	if err != nil {
		return ""
	}
	return args["Action"]
}

// readQueryArgs parses the Query body and rewinds it, so the same bytes can be
// read again. Any read or parse failure is a malformed query string.
func readQueryArgs(r *http.Request) (map[string]string, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, sigv4.MaxPayloadLen+1))
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if int64(len(body)) > sigv4.MaxPayloadLen {
		return nil, errors.New(awserrors.ErrorRequestEntityTooLarge)
	}
	return query.ParseArgs(string(body))
}

// CallerFromActor projects the verified actor onto the RDS caller. The role name
// comes from the underlying role ARN, never the session name, which the caller
// picks at AssumeRole time and could name its way past the agent gate.
func CallerFromActor(actor dispatch.Actor) Caller {
	name := actor.UserName
	if actor.PrincipalType == principalTypeAssumedRole {
		name = actor.SessionName
	}
	return Caller{
		AccountID:     actor.AccountID,
		PrincipalType: actor.PrincipalType,
		SessionName:   name,
		RoleName:      actor.RoleName(),
	}
}

// dispatch parses, scopes and authorizes the request, then runs the action.
// The principal-class gate runs before the policy check so no customer grant is
// ever evaluated against an internal agent action. Errors are awserrors codes
// the gateway renders.
func (d Deps) dispatch(w http.ResponseWriter, inv dispatch.Invocation) error {
	r := inv.Request
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
	if !HasAction(action) {
		slog.DebugContext(r.Context(), "RDS: unknown action", "action", action)
		return errors.New(awserrors.ErrorInvalidAction)
	}

	if inv.AccountID == "" {
		slog.ErrorContext(r.Context(), "RDS dispatch: no account ID in auth context")
		return errors.New(awserrors.ErrorServerInternal)
	}
	// An error means no caller ARN could be composed; the actor is then empty,
	// which no internal action admits and the policy check below refuses.
	actor, _ := inv.Actor()
	caller := CallerFromActor(actor)
	caller.AccountID = inv.AccountID

	if err := AuthorizeCaller(r.Context(), action, caller); err != nil {
		return err
	}

	// inv.Region is the gateway's own region, never the caller-supplied
	// credential-scope region, which would let a caller sign for another region
	// and slide out from under a Deny.
	resources, err := ResourceARN(action, inv.Region, caller.AccountID, queryArgs)
	if err != nil {
		return err
	}
	if err := inv.Authorize(ServiceName, action, resources, nil); err != nil {
		return err
	}

	if d.NATS == nil {
		return errors.New(awserrors.ErrorServerInternal)
	}

	env := Env{ExpectedNodes: d.ExpectedNodes}
	if d.Quota != nil {
		env.QuotaCheck = func(ctx context.Context, accountID string, want int) error {
			return d.Quota.EnforceRDSInstances(ctx, d.NATS, accountID, want)
		}
	}
	xmlOutput, err := Dispatch(r.Context(), action, queryArgs, d.NATS, caller, env)
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
