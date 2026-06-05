package gateway

import (
	"bytes"
	"context"
	"io"
	"net/http"
)

// anonymousSTSInterceptor routes the credential-bootstrap STS actions
// (AssumeRoleWithWebIdentity) to the STS dispatcher ahead of the SigV4
// middleware. These calls carry no AWS credentials — the caller is
// authenticated by the projected ServiceAccount JWT in the request body — so
// the signed surface would reject them as unauthenticated. Requests bearing an
// Authorization header skip this path untouched, keeping the signed hot path
// free of body buffering.
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

// anonymousSTSArgs buffers and restores the request body, parses the AWS query
// args, and reports whether Action is an STS action permitted without SigV4. A
// non-anonymous request flows on unchanged with its body intact.
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
