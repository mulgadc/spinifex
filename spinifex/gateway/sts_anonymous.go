package gateway

import (
	"bytes"
	"context"
	"io"
	"net/http"
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
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	args, err := ParseAWSQueryArgs(string(body))
	if err != nil || !anonymousSTSActions[args["Action"]] {
		return nil, false
	}
	return args, true
}
