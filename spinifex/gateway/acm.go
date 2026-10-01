package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	acmawsapi "github.com/mulgadc/spinifex/spinifex/domains/acm/awsapi"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// acmHandler invokes a per-action ACM domain adapter after generic gateway
// authentication and authorization have completed.
type acmHandler func(ctx context.Context, gw *GatewayConfig, accountID string, body []byte) (any, error)

// acmActions maps the action suffix of X-Amz-Target (CertificateManager.<Action>) to its handler.
var acmActions = map[string]acmHandler{
	"ImportCertificate": func(ctx context.Context, gw *GatewayConfig, acct string, b []byte) (any, error) {
		return acmawsapi.ImportCertificate(ctx, gw.NATSConn, acct, b)
	},
	"RequestCertificate": func(ctx context.Context, gw *GatewayConfig, acct string, b []byte) (any, error) {
		return acmawsapi.RequestCertificate(ctx, gw.NATSConn, acct, b)
	},
	"DescribeCertificate": func(ctx context.Context, gw *GatewayConfig, acct string, b []byte) (any, error) {
		return acmawsapi.DescribeCertificate(ctx, gw.NATSConn, acct, b)
	},
	"GetCertificate": func(ctx context.Context, gw *GatewayConfig, acct string, b []byte) (any, error) {
		return acmawsapi.GetCertificate(ctx, gw.NATSConn, acct, b)
	},
	"ListCertificates": func(ctx context.Context, gw *GatewayConfig, acct string, b []byte) (any, error) {
		return acmawsapi.ListCertificates(ctx, gw.NATSConn, acct, b)
	},
	"DeleteCertificate": func(ctx context.Context, gw *GatewayConfig, acct string, b []byte) (any, error) {
		return acmawsapi.DeleteCertificate(ctx, gw.NATSConn, acct, b)
	},
	"ListTagsForCertificate": func(ctx context.Context, gw *GatewayConfig, acct string, b []byte) (any, error) {
		return acmawsapi.ListTagsForCertificate(ctx, gw.NATSConn, acct, b)
	},
	"AddTagsToCertificate": func(ctx context.Context, gw *GatewayConfig, acct string, b []byte) (any, error) {
		return acmawsapi.AddTagsToCertificate(ctx, gw.NATSConn, acct, b)
	},
	"RemoveTagsFromCertificate": func(ctx context.Context, gw *GatewayConfig, acct string, b []byte) (any, error) {
		return acmawsapi.RemoveTagsFromCertificate(ctx, gw.NATSConn, acct, b)
	},
}

// acmActionFromTarget extracts the action suffix from an X-Amz-Target header.
// Any "<Prefix>.<Action>" or bare "<Action>" form is accepted.
func acmActionFromTarget(target string) string {
	if i := strings.LastIndex(target, "."); i >= 0 {
		return target[i+1:]
	}
	return target
}

// ACM_Request dispatches AWS JSON 1.1 ACM requests. The action comes from
// X-Amz-Target; errors are returned as awserrors codes.
func (gw *GatewayConfig) ACM_Request(w http.ResponseWriter, r *http.Request) error {
	action := acmActionFromTarget(r.Header.Get("X-Amz-Target"))
	if action == "" {
		return errors.New(awserrors.ErrorMissingAction)
	}

	handler, ok := acmActions[action]
	if !ok {
		slog.DebugContext(r.Context(), "ACM: unknown action", "action", action)
		return errors.New(awserrors.ErrorInvalidAction)
	}

	// Hoisted above the policy check because the resolver builds ARNs from the
	// caller's account and from the same body bytes the handler unmarshals.
	accountID, _ := r.Context().Value(ctxAccountID).(string)
	if accountID == "" {
		slog.ErrorContext(r.Context(), "ACM_Request: no account ID in auth context")
		// InternalError, not ServerInternal: the policy gate used to reach this
		// case first and that is the code the caller has always seen.
		return errors.New(awserrors.ErrorInternalError)
	}

	body, err := readBoundedBody(r)
	if err != nil {
		slog.ErrorContext(r.Context(), "ACM_Request: failed to read body", "err", err)
		return err
	}

	resources, err := acmawsapi.ResourceARNs(action, gw.Region, accountID, body)
	if err != nil {
		return err
	}
	if err := gw.checkPolicyResources(r, "acm", action, resources); err != nil {
		return err
	}

	if gw.NATSConn == nil {
		return errors.New(awserrors.ErrorServerInternal)
	}

	output, err := handler(r.Context(), gw, accountID, body)
	if err != nil {
		return err
	}

	acmawsapi.WriteJSONResponse(w, output)
	return nil
}
