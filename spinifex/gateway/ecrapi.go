package gateway

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	awsapi "github.com/mulgadc/spinifex/spinifex/domains/ecr/awsapi"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// ecrActionFromTarget extracts the action suffix from an X-Amz-Target header.
// Any "<Prefix>.<Action>" or bare "<Action>" form is accepted.
func ecrActionFromTarget(target string) string {
	if i := strings.LastIndex(target, "."); i >= 0 {
		return target[i+1:]
	}
	return target
}

// ECR_Request dispatches AWS JSON 1.1 ECR control-plane requests. The action
// comes from X-Amz-Target; errors are returned as awserrors codes and rendered
// by the shared ErrorHandler.
func (gw *GatewayConfig) ECR_Request(w http.ResponseWriter, r *http.Request) error {
	action := ecrActionFromTarget(r.Header.Get("X-Amz-Target"))
	if action == "" {
		return errors.New(awserrors.ErrorMissingAction)
	}

	handler, ok := awsapi.Actions[action]
	if !ok {
		slog.Debug("ECR: unknown action", "action", action)
		return errors.New(awserrors.ErrorInvalidAction)
	}

	// Hoisted above the policy check because the resolver builds ARNs from the
	// caller's account and from the same body bytes the handler unmarshals.
	accountID, _ := r.Context().Value(ctxAccountID).(string)
	if accountID == "" {
		slog.Error("ECR_Request: no account ID in auth context")
		// InternalError, not ServerInternal: the policy gate used to reach this
		// case first and that is the code the caller has always seen.
		return errors.New(awserrors.ErrorInternalError)
	}

	body, err := readBoundedBody(r)
	if err != nil {
		slog.Error("ECR_Request: failed to read body", "err", err)
		return err
	}

	resources, err := awsapi.ResourceARNs(action, gw.Region, accountID, body)
	if err != nil {
		return err
	}
	if err := gw.checkPolicyResources(r, "ecr", action, resources); err != nil {
		return err
	}

	if awsapi.IsRegistryAction(action) {
		if gw.ECRRegistryActions == nil {
			slog.Error("ECR registry action: OCI registry capability not configured", "action", action)
			return errors.New(awserrors.ErrorServerInternal)
		}
		output, err := gw.ECRRegistryActions.Execute(r.Context(), action, accountID, body)
		if err != nil {
			return err
		}
		awsapi.WriteJSONResponse(w, output)
		return nil
	}
	if awsapi.IsLifecyclePreviewAction(action) {
		if gw.ECRLifecyclePreview == nil {
			slog.Error("ECR lifecycle-preview action: capabilities not configured", "action", action)
			return errors.New(awserrors.ErrorServerInternal)
		}
		output, err := gw.ECRLifecyclePreview.Execute(r.Context(), action, accountID, body)
		if err != nil {
			return err
		}
		awsapi.WriteJSONResponse(w, output)
		return nil
	}
	if awsapi.IsRepositoryAction(action) {
		if gw.ECRRepositoryActions == nil {
			slog.Error("ECR repository action: capabilities not configured", "action", action)
			return errors.New(awserrors.ErrorServerInternal)
		}
		output, err := gw.ECRRepositoryActions.Execute(r.Context(), action, accountID, body)
		if err != nil {
			return err
		}
		awsapi.WriteJSONResponse(w, output)
		return nil
	}
	if awsapi.IsAuthorizationTokenAction(action) {
		if gw.ECRTokenAction == nil {
			slog.Error("ECR authorization-token action: capabilities not configured")
			return errors.New(awserrors.ErrorServerInternal)
		}
		principal, err := ecrAuthorizationPrincipal(r, accountID)
		if err != nil {
			slog.Error("GetAuthorizationToken: cannot build canonical caller ARN", "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}
		output, err := gw.ECRTokenAction.MintFor(principal)
		if err != nil {
			return err
		}
		awsapi.WriteJSONResponse(w, output)
		return nil
	}

	output, err := handler(r.Context(), gw.NATSConn, accountID, body)
	if err != nil {
		return err
	}

	awsapi.WriteJSONResponse(w, output)
	return nil
}
