package gateway

import "net/http"

// hostMatch is the skeleton for the ECR registry host-routing middleware. The
// registry surface (/v2/*) is addressed at {accountID}.dkr.ecr.{region}.{suffix};
// once the auth bridge lands this middleware will parse the request Host,
// extract the target accountID, and stash it on the request context for the
// registry handlers and auth bridge.
//
// It is currently a transparent pass-through so the /v2 route group has a stable
// seam to grow into without yet changing request handling.
func hostMatch(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
	})
}
