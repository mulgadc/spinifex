package gateway

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_acm "github.com/mulgadc/spinifex/spinifex/gateway/acm"
)

// acmHandler invokes a per-action ACM gateway function.
type acmHandler func(gw *GatewayConfig, accountID string, body []byte) (any, error)

// acmActions maps the action suffix of X-Amz-Target (CertificateManager.<Action>) to its handler.
var acmActions = map[string]acmHandler{
	"ImportCertificate": func(gw *GatewayConfig, acct string, b []byte) (any, error) {
		return gateway_acm.ImportCertificate(gw.NATSConn, acct, b)
	},
	"DescribeCertificate": func(gw *GatewayConfig, acct string, b []byte) (any, error) {
		return gateway_acm.DescribeCertificate(gw.NATSConn, acct, b)
	},
	"ListCertificates": func(gw *GatewayConfig, acct string, b []byte) (any, error) {
		return gateway_acm.ListCertificates(gw.NATSConn, acct, b)
	},
	"DeleteCertificate": func(gw *GatewayConfig, acct string, b []byte) (any, error) {
		return gateway_acm.DeleteCertificate(gw.NATSConn, acct, b)
	},
	"ListTagsForCertificate": func(gw *GatewayConfig, acct string, b []byte) (any, error) {
		return gateway_acm.ListTagsForCertificate(gw.NATSConn, acct, b)
	},
	"AddTagsToCertificate": func(gw *GatewayConfig, acct string, b []byte) (any, error) {
		return gateway_acm.AddTagsToCertificate(gw.NATSConn, acct, b)
	},
	"RemoveTagsFromCertificate": func(gw *GatewayConfig, acct string, b []byte) (any, error) {
		return gateway_acm.RemoveTagsFromCertificate(gw.NATSConn, acct, b)
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
		slog.Debug("ACM: unknown action", "action", action)
		return errors.New(awserrors.ErrorInvalidAction)
	}

	if err := gw.checkPolicy(r, "acm", action); err != nil {
		return err
	}

	if gw.NATSConn == nil {
		return errors.New(awserrors.ErrorServerInternal)
	}

	accountID, _ := r.Context().Value(ctxAccountID).(string)
	if accountID == "" {
		slog.Error("ACM_Request: no account ID in auth context")
		return errors.New(awserrors.ErrorServerInternal)
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("ACM_Request: failed to read body", "err", err)
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	output, err := handler(gw, accountID, body)
	if err != nil {
		return err
	}

	gateway_acm.WriteJSONResponse(w, output)
	return nil
}
