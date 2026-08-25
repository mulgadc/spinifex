package gateway

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_ecrapi "github.com/mulgadc/spinifex/spinifex/gateway/ecrapi"
)

type ecrInlineHandler func(*GatewayConfig, http.ResponseWriter, *http.Request) error

// ecrInlineActions is the authoritative inventory of ECR operations handled
// directly by the gateway rather than relayed through gateway_ecrapi.Actions.
// Keeping dispatch and coverage on the same map prevents the generated
// operation report from classifying an inline implementation as a stub.
var ecrInlineActions = map[string]ecrInlineHandler{
	"GetAuthorizationToken":       (*GatewayConfig).handleGetAuthorizationToken,
	"DescribeRepositories":        (*GatewayConfig).handleDescribeRepositories,
	"CreateRepository":            (*GatewayConfig).handleCreateRepository,
	"DeleteRepository":            (*GatewayConfig).handleDeleteRepository,
	"PutImageTagMutability":       (*GatewayConfig).handlePutImageTagMutability,
	"ListImages":                  (*GatewayConfig).handleListImages,
	"DescribeImages":              (*GatewayConfig).handleDescribeImages,
	"BatchGetImage":               (*GatewayConfig).handleBatchGetImage,
	"PutImage":                    (*GatewayConfig).handlePutImage,
	"BatchDeleteImage":            (*GatewayConfig).handleBatchDeleteImage,
	"StartLifecyclePolicyPreview": (*GatewayConfig).handleStartLifecyclePolicyPreview,
	"GetLifecyclePolicyPreview":   (*GatewayConfig).handleGetLifecyclePolicyPreview,
}

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

	handler, ok := gateway_ecrapi.Actions[action]
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

	resources, err := gateway_ecrapi.ResourceARNs(action, gw.Region, accountID, body)
	if err != nil {
		return err
	}
	if err := gw.checkPolicyResources(r, "ecr", action, resources); err != nil {
		return err
	}

	if inline, ok := ecrInlineActions[action]; ok {
		// The inline handlers read r.Body themselves, so it is rewound over the
		// bytes the gate consumed, the same discipline the auth middleware uses.
		r.Body = io.NopCloser(bytes.NewReader(body))
		return inline(gw, w, r)
	}

	output, err := handler(r.Context(), gw.NATSConn, accountID, body)
	if err != nil {
		return err
	}

	gateway_ecrapi.WriteJSONResponse(w, output)
	return nil
}
