package awsapi

import (
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	"github.com/mulgadc/bluebottle/pkg/sigv4"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
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
}

// NewRegistration returns the ECS AWS JSON 1.1 control-plane registration.
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
	return dispatch.Inventory{
		Registered: slices.Sorted(maps.Keys(Actions)),
		Stubbed:    StubbedActionNames(),
	}
}

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

// dispatch resolves, scopes and authorizes the request, then runs the action.
// Errors are awserrors codes the gateway renders.
func (d Deps) dispatch(w http.ResponseWriter, inv dispatch.Invocation) error {
	r := inv.Request
	action := resolveAction(r)
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

	body, err := readBoundedBody(r)
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
	passRoleCheck := func(roleARN string) error {
		return inv.Authorize("iam", "PassRole", []string{roleARN},
			iampolicy.ConditionKeys{iampolicy.KeyPassedToService: tasksServicePrincipal})
	}

	output, err := handler(r.Context(), d.NATS, accountID, body, passRoleCheck)
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
