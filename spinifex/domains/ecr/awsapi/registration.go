package awsapi

import (
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/mulgadc/bluebottle/pkg/sigv4"
	ecrauth "github.com/mulgadc/spinifex/spinifex/domains/ecr/auth"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/nats-io/nats.go"
)

// ServiceName is the SigV4 credential-scope service ECR clients sign for.
const ServiceName = "ecr"

// Deps are the composed capabilities the ECR control plane dispatches to. A
// nil capability answers its actions with ServerInternal; NATS is the
// connection the relay handlers in Actions publish on.
type Deps struct {
	Registry           *RegistryActionService
	LifecyclePreview   *LifecyclePreviewActionService
	Repository         *RepositoryActionService
	AuthorizationToken *AuthorizationTokenActionService
	NATS               *nats.Conn
}

// NewRegistration returns the ECR AWS JSON 1.1 control-plane registration.
func NewRegistration(deps Deps) dispatch.Registration {
	return dispatch.Registration{
		Service:       ServiceName,
		ResolveAction: resolveAction,
		Dispatch:      deps.dispatch,
		Errors:        dispatch.ErrorEnvelopeJSON,
		Inventory:     OperationInventory(),
	}
}

// OperationInventory classifies the actions the registration serves. A stub
// a composed capability intercepts is implemented, not stubbed.
func OperationInventory() dispatch.Inventory {
	composed := union(
		union(RegistryActionNames(), LifecyclePreviewActionNames()),
		union(RepositoryActionNames(), AuthorizationTokenActionNames()),
	)
	return dispatch.Inventory{
		Registered:  union(slices.Sorted(maps.Keys(Actions)), composed),
		Stubbed:     without(StubbedActionNames(), composed),
		Unsupported: UnsupportedActionNames(),
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
		slog.Debug("ECR: unknown action", "action", action)
		return errors.New(awserrors.ErrorInvalidAction)
	}

	accountID := inv.AccountID
	if accountID == "" {
		slog.Error("ECR dispatch: no account ID in auth context")
		// InternalError, not ServerInternal: the policy gate used to reach this
		// case first and that is the code the caller has always seen.
		return errors.New(awserrors.ErrorInternalError)
	}

	body, err := readBoundedBody(r)
	if err != nil {
		slog.Error("ECR dispatch: failed to read body", "err", err)
		return err
	}

	resources, err := ResourceARNs(action, inv.Region, accountID, body)
	if err != nil {
		return err
	}
	if err := inv.Authorize(ServiceName, action, resources, nil); err != nil {
		return err
	}

	var output any
	switch {
	case IsRegistryAction(action):
		if d.Registry == nil {
			slog.Error("ECR registry action: OCI registry capability not configured", "action", action)
			return errors.New(awserrors.ErrorServerInternal)
		}
		output, err = d.Registry.Execute(r.Context(), action, accountID, body)
	case IsLifecyclePreviewAction(action):
		if d.LifecyclePreview == nil {
			slog.Error("ECR lifecycle-preview action: capabilities not configured", "action", action)
			return errors.New(awserrors.ErrorServerInternal)
		}
		output, err = d.LifecyclePreview.Execute(r.Context(), action, accountID, body)
	case IsRepositoryAction(action):
		if d.Repository == nil {
			slog.Error("ECR repository action: capabilities not configured", "action", action)
			return errors.New(awserrors.ErrorServerInternal)
		}
		output, err = d.Repository.Execute(r.Context(), action, accountID, body)
	case IsAuthorizationTokenAction(action):
		if d.AuthorizationToken == nil {
			slog.Error("ECR authorization-token action: capabilities not configured")
			return errors.New(awserrors.ErrorServerInternal)
		}
		principal, perr := tokenPrincipal(inv)
		if perr != nil {
			slog.Error("GetAuthorizationToken: cannot build canonical caller ARN", "err", perr)
			return errors.New(awserrors.ErrorServerInternal)
		}
		output, err = d.AuthorizationToken.MintFor(principal)
	default:
		output, err = handler(r.Context(), d.NATS, accountID, body)
	}
	if err != nil {
		return err
	}

	WriteJSONResponse(w, output)
	return nil
}

// tokenPrincipal is the verified caller a GetAuthorizationToken response
// signs for. It comes only from ingress authentication, never the body.
func tokenPrincipal(inv dispatch.Invocation) (ecrauth.Principal, error) {
	actor, err := inv.Actor()
	if err != nil {
		return ecrauth.Principal{}, err
	}
	return ecrauth.Principal{
		AccountID:   actor.AccountID,
		ARN:         actor.CallerARN,
		Type:        actor.PrincipalType,
		AccessKeyID: actor.AccessKeyID,
	}, nil
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

func without(values, excluded []string) []string {
	return slices.DeleteFunc(slices.Clone(values), func(value string) bool {
		return slices.Contains(excluded, value)
	})
}

func union(left, right []string) []string {
	merged := slices.Concat(left, right)
	slices.Sort(merged)
	return slices.Compact(merged)
}
