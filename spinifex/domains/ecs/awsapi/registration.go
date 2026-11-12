package awsapi

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"slices"

	ecsdomain "github.com/mulgadc/spinifex/spinifex/domains/ecs"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/passrole"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/nats-io/nats.go"
)

// ServiceName is the SigV4 credential-scope service ECS clients sign for.
const ServiceName = "ecs"

// tasksServicePrincipal is the service a task role is passed to.
const tasksServicePrincipal = "ecs-tasks.amazonaws.com"

// Deps are the composed capabilities the ECS control plane dispatches to. A nil
// NATS connection fails the actions that reach the daemon.
type Deps struct {
	NATS *nats.Conn
	// Roles resolves a role a RunTask override names. Nil leaves a missing
	// override role undetected.
	Roles passrole.Resolver
}

// NewRegistration returns the ECS AWS JSON 1.1 control-plane registration.
func NewRegistration(deps Deps) dispatch.Registration {
	return dispatch.Registration{
		Service:       ServiceName,
		ResolveAction: dispatch.ResolveTargetAction,
		Dispatch:      deps.dispatch,
		Errors:        dispatch.ErrorEnvelopeJSON,
		Inventory:     OperationInventory(),
	}
}

// OperationInventory classifies the actions the registration serves.
func OperationInventory() dispatch.Inventory {
	return dispatch.Inventory{
		Registered: slices.Sorted(maps.Keys(Actions)),
		Stubbed:    StubbedActionNames(),
	}
}

// dispatch resolves, scopes and authorizes the request, then runs the action.
// Errors are awserrors codes the gateway renders.
func (d Deps) dispatch(w http.ResponseWriter, inv dispatch.Invocation) error {
	r := inv.Request
	action := dispatch.ResolveTargetAction(r)
	if action == "" {
		return errors.New(awserrors.ErrorMissingAction)
	}

	handler, ok := Actions[action]
	if !ok {
		slog.DebugContext(r.Context(), "ECS: unknown action", "action", action)
		return errors.New(awserrors.ErrorInvalidAction)
	}

	// Ahead of the policy check because the resolver builds ARNs from the
	// caller's account and from the same body bytes the handler unmarshals.
	accountID := inv.AccountID
	if accountID == "" {
		slog.ErrorContext(r.Context(), "ECS dispatch: no account ID in auth context")
		// InternalError, not ServerInternal: the policy gate used to reach this
		// case first and that is the code the caller has always seen.
		return errors.New(awserrors.ErrorInternalError)
	}

	body, err := dispatch.ReadBoundedBody(r)
	if err != nil {
		slog.ErrorContext(r.Context(), "ECS dispatch: failed to read body", "err", err)
		return err
	}

	resources, err := ResourceARNs(action, inv.Region, accountID, body)
	if err != nil {
		return err
	}
	if err := inv.Authorize(ServiceName, action, resources, nil); err != nil {
		return err
	}

	// iam:PassRole is enforced on the role ARN as written, for whichever role
	// the action later names.
	passRoleCheck := roleChecker(accountRoles{}, passrole.AuthorizeFunc(inv.Authorize), accountID, nil)
	var roles passrole.Resolver = accountRoles{}
	if d.Roles != nil {
		roles = d.Roles
	}
	ctx := withOverrideRoleCheck(r.Context(), roleChecker(roles, passrole.AuthorizeFunc(inv.Authorize), accountID, unableToAssume))
	ctx = ecsdomain.WithCallerARN(ctx, creatorARN(r.Context(), action, inv.Actor))

	output, err := handler(ctx, d.NATS, accountID, body, passRoleCheck)
	if err != nil {
		return err
	}

	if RawJSONActions[action] {
		WriteRawJSONResponse(w, output)
		return nil
	}
	WriteJSONResponse(w, output)
	return nil
}

// creatorARN is the ARN AWS records as the creator of what action creates: a
// service's createdBy names the IAM role, a task definition's registeredBy the
// session. An actor error only means the session ARN is empty, so none is recorded.
func creatorARN(ctx context.Context, action string, resolve dispatch.ActorFunc) string {
	if action != "CreateService" && action != "RegisterTaskDefinition" || resolve == nil {
		return ""
	}
	actor, err := resolve()
	if err != nil {
		slog.DebugContext(ctx, "ECS dispatch: caller ARN unresolved", "err", err)
	}
	if action == "CreateService" {
		return actor.PrincipalARN
	}
	return actor.CallerARN
}
