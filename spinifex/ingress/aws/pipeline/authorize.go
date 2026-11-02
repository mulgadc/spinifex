package pipeline

import (
	"errors"
	"log/slog"
	"maps"
	"net/http"

	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/policy"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/credential"
)

// Authorize is the authorization gate for one request: the request's condition
// keys with the action's own layered over them, evaluated for the caller against
// every resource. A missing evaluator, an unauthenticated or unnamed caller, and
// a caller with no account all fail closed.
func Authorize(
	p PolicyEvaluator, r *http.Request, caller credential.Principal, authenticated bool,
	service, action string, resources []string, actionKeys iampolicy.ConditionKeys,
) error {
	if p == nil {
		slog.Error("checkPolicy: policy evaluator not available", "service", service, "action", action)
		return errors.New(awserrors.ErrorInternalError)
	}
	if !authenticated {
		slog.Warn("checkPolicy: request carries no auth context", "service", service, "action", action)
		return errors.New(awserrors.ErrorAccessDenied)
	}
	if caller.Identity == "" {
		slog.Warn("checkPolicy: authenticated request carries no identity name",
			"service", service, "action", action)
		return errors.New(awserrors.ErrorAccessDenied)
	}
	if caller.AccountID == "" {
		slog.Error("checkPolicy: no account ID in auth context", "user", caller.Identity)
		return errors.New(awserrors.ErrorInternalError)
	}

	keys := p.RequestConditionKeys(r, caller)
	maps.Copy(keys, actionKeys)
	return p.Evaluate(caller, policy.IAMAction(service, action), resources, keys)
}
