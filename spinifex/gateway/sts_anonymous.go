package gateway

import (
	"bytes"
	"context"
	"io"
	"net/http"

	"github.com/mulgadc/predastore/pkg/sigv4"
)

// anonymousSTSInterceptor routes unsigned IRSA bootstrap actions
// (AssumeRoleWithWebIdentity) to the STS dispatcher before the SigV4 middleware.
// Callers carry no AWS credentials — authentication is via the ServiceAccount JWT
// in the body. Signed requests (Authorization header present) pass through unchanged.
func (gw *GatewayConfig) anonymousSTSInterceptor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.Header.Get("Authorization") == "" {
			if args, ok := gw.anonymousSTSArgs(r); ok {
				ctx := context.WithValue(r.Context(), ctxQueryArgs, args)
				ctx = context.WithValue(ctx, ctxService, "sts")
				r = r.WithContext(ctx)
				if err := gw.STS_Request(w, r); err != nil {
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
