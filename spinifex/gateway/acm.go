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

// acmHandler invokes a per-action gateway function: it receives the parent
// gateway config, the caller's accountID, and the raw request body, and
// returns a typed *acm.<X>Output (as any) or an error whose Error() is an
// awserrors code.
type acmHandler func(gw *GatewayConfig, accountID string, body []byte) (any, error)

// acmActions maps an ACM action (the suffix of the X-Amz-Target header) to its
// handler. ACM speaks AWS JSON 1.1: the action is carried in
// "X-Amz-Target: CertificateManager.<Action>", not the query string.
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
}

// acmActionFromTarget extracts the bare action name from an X-Amz-Target
// header value of the form "CertificateManager.<Action>". The service prefix
// is optional/ignored so any "<Prefix>.<Action>" or bare "<Action>" resolves.
func acmActionFromTarget(target string) string {
	if i := strings.LastIndex(target, "."); i >= 0 {
		return target[i+1:]
	}
	return target
}

// ACM_Request dispatches AWS JSON 1.1 ACM requests. The action is read from
// the X-Amz-Target header; the body is the JSON-encoded operation input.
// Errors are returned as plain awserrors codes; the caller (gateway.Request)
// routes them through ErrorHandler, which emits the JSON error envelope.
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
