package awsapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/mulgadc/bluebottle/pkg/sigv4"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/passrole"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/nats-io/nats.go"
)

// ServiceName is the SigV4 credential-scope service EKS clients sign for.
const ServiceName = "eks"

// servicePrincipal is the service a cluster, node or add-on role is passed to.
const servicePrincipal = "eks.amazonaws.com"

// Deps are the composed capabilities the EKS control plane dispatches to. A
// nil NATS connection fails the actions that reach the daemon.
type Deps struct {
	NATS  *nats.Conn
	Roles passrole.Resolver
}

// NewRegistration returns the EKS REST-JSON control-plane registration.
func NewRegistration(deps Deps) dispatch.Registration {
	return dispatch.Registration{
		Service:       ServiceName,
		ResolveAction: resolveAction,
		Dispatch:      deps.dispatch,
		Errors:        dispatch.ErrorEnvelopeJSON,
		Inventory:     OperationInventory(),
	}
}

// OperationInventory classifies the actions the registration serves.
func OperationInventory() dispatch.Inventory {
	return dispatch.Inventory{Registered: actionNames()}
}

func resolveAction(r *http.Request) string {
	action, _, _, _ := router.Lookup(r.Method, r.URL.EscapedPath())
	return action
}

// CallerFromActor projects the verified actor onto the EKS caller. The role name
// comes from the underlying role ARN, never the session name, which the caller
// picks at AssumeRole time and could name its way past the gate.
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

// dispatch resolves the route, authorizes the request, then runs the action.
// Errors are awserrors codes the gateway renders.
func (d Deps) dispatch(w http.ResponseWriter, inv dispatch.Invocation) error {
	r := inv.Request
	action, params, handler, ok := router.Lookup(r.Method, r.URL.EscapedPath())
	if !ok {
		slog.DebugContext(r.Context(), "EKS: no route for request", "method", r.Method, "path", r.URL.Path)
		return errors.New(awserrors.ErrorInvalidAction)
	}

	// Ahead of the policy check because the resolver builds ARNs from it. The
	// region is the gateway's own, never the caller-supplied credential-scope
	// region, which would let a caller sign for another region and slide out
	// from under a Deny.
	accountID := inv.AccountID
	if accountID == "" {
		slog.ErrorContext(r.Context(), "EKS dispatch: no account ID in auth context")
		// InternalError, not ServerInternal: the policy gate used to reach this
		// case first and that is the code the caller has always seen.
		return errors.New(awserrors.ErrorInternalError)
	}

	body, err := readBoundedBody(r)
	if err != nil {
		slog.ErrorContext(r.Context(), "EKS dispatch: failed to read body", "err", err)
		return err
	}

	// Some REST-JSON actions carry their non-path inputs as query params with
	// an empty body (UntagResource's tagKeys arrive as
	// DELETE /tags/{arn}?tagKeys=k1&tagKeys=k2). url.Values marshals straight to
	// the JSON the per-action unmarshal expects. Only folds when the body is
	// empty so it never shadows a real payload.
	if len(body) == 0 {
		if q := r.URL.Query(); len(q) > 0 {
			if qb, err := json.Marshal(map[string][]string(q)); err == nil {
				body = qb
			}
		}
	}

	actor, actorErr := inv.Actor()

	// Ahead of the policy check: the internal routes name the target account in
	// the path or body, so an eks:* grant evaluates as permitted and only the
	// principal class plus the caller's own instance say whether it is its own.
	if IsInternalAction(action) {
		if err := AuthorizeInternal(r.Context(), d.NATS, action, CallerFromActor(actor), params, body); err != nil {
			return err
		}
	}

	// After the body read: CreateCluster and the other create actions name their
	// resource there rather than in the path.
	resources, err := ResourceARNs(action, inv.Region, accountID, params, body)
	if err != nil {
		return err
	}
	if err := inv.Authorize(ServiceName, action, resources, nil); err != nil {
		return err
	}
	if err := d.checkPassRole(inv, action, accountID, body); err != nil {
		return err
	}

	if d.NATS == nil {
		return errors.New(awserrors.ErrorServerInternal)
	}

	// Best-effort caller ARN; only CreateCluster consumes it, degrades to "".
	callerARN := actor.CallerARN
	if actorErr != nil {
		slog.DebugContext(r.Context(), "EKS dispatch: could not resolve caller principal ARN", "err", actorErr)
		callerARN = ""
	}

	output, err := handler(r.Context(), d.NATS, accountID, callerARN, params, body)
	if err != nil {
		return err
	}

	WriteJSONResponse(w, output)
	return nil
}

// checkPassRole enforces iam:PassRole on each role the action hands to EKS. Only
// the exact ARN the store holds resolves, so the grant is evaluated against the
// role's real path and an invented one cannot match a narrower Resource.
func (d Deps) checkPassRole(inv dispatch.Invocation, action, accountID string, body []byte) error {
	for _, roleARN := range PassedRoleARNs(action, body) {
		_, err := passrole.Check(d.Roles, passrole.AuthorizeFunc(inv.Authorize), accountID, roleARN, servicePrincipal)
		switch {
		case err == nil:
		case errors.Is(err, passrole.ErrMalformed), errors.Is(err, passrole.ErrForeignAccount):
			if action == "CreateCluster" || action == "CreateNodegroup" {
				return awserrors.Errorf(awserrors.ErrorAccessDeniedException, "Cross-account pass role is not allowed.")
			}
			return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "%s is not a role ARN in account %s", roleARN, accountID)
		case errors.Is(err, passrole.ErrNotFound):
			return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "role %s does not exist", roleARN)
		default:
			return err
		}
	}
	return nil
}
