package gateway

import (
	"bytes"
	"errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/reqctx"
	"io"
	"log/slog"
	"net/http"

	"github.com/mulgadc/bluebottle/pkg/sigv4"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// anonymousSTSInterceptor routes unsigned IRSA bootstrap actions
// (AssumeRoleWithWebIdentity) to the STS dispatcher before the SigV4 middleware.
// Callers carry no AWS credentials — authentication is via the ServiceAccount JWT
// in the body. Signed requests (Authorization header present) pass through unchanged.
func (gw *GatewayConfig) anonymousSTSInterceptor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.Header.Get("Authorization") == "" {
			if args, ok := gw.anonymousSTSArgs(r); ok {
				r = r.WithContext(reqctx.WithService(r.Context(), "sts"))
				if err := gw.serveAnonymousSTS(w, args); err != nil {
					gw.ErrorHandler(w, r, err)
				}
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// anonymousSTSArgs peeks the request body, parses query args, and reports
// whether the Action is permitted without SigV4. Non-anonymous requests pass through unchanged.
func (gw *GatewayConfig) anonymousSTSArgs(r *http.Request) (map[string]string, bool) {
	// This runs ahead of SigV4, so an unauthenticated client controls the
	// allocation. Read one byte past the cap to detect an oversized body and
	// hand it back to the normal chain unread rather than dispatching it.
	body, err := io.ReadAll(io.LimitReader(r.Body, sigv4.MaxPayloadLen+1))
	if err != nil {
		return nil, false
	}
	if int64(len(body)) > sigv4.MaxPayloadLen {
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), r.Body))
		return nil, false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	args, err := ParseAWSQueryArgs(string(body))
	if err != nil || !anonymousSTSActions[args["Action"]] {
		return nil, false
	}
	return args, true
}

// anonymousSTSActions lists STS actions that carry no SigV4; authenticated by
// a web-identity JWT instead. anonymousSTSInterceptor routes these before auth.
var anonymousSTSActions = map[string]bool{
	"AssumeRoleWithWebIdentity": true,
}

// serveAnonymousSTS runs an unsigned STS action through the composed
// capability and writes its XML response.
func (gw *GatewayConfig) serveAnonymousSTS(w http.ResponseWriter, args map[string]string) error {
	if gw.AnonymousSTS == nil {
		slog.Error("STS: anonymous service not initialized")
		return errors.New(awserrors.ErrorInternalError)
	}
	xmlOutput, err := gw.AnonymousSTS(args)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(xmlOutput); err != nil {
		slog.Error("Failed to write STS response", "err", err)
	}
	return nil
}
