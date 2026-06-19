package gateway

import (
	"github.com/go-chi/chi/v5"

	gateway_ecr "github.com/mulgadc/spinifex/spinifex/gateway/ecr"
)

// mountOCIRegistry registers the OCI Distribution Spec v2 surface (/v2/*) on r.
// These endpoints are host- and token-authenticated rather than
// SigV4-credential-scoped, so they mount outside the SigV4 group behind the
// hostMatch host-routing middleware.
//
// OCI repository names may contain slashes (e.g. team/app), so the blob,
// manifest and tag paths can't be expressed as fixed chi segments. The
// Registry parses the path manually. GET /v2/ is always live so the registry
// version-check probe succeeds even before any repository exists. When no
// Registry is wired (e.g. unit tests of unrelated routes), the surface falls
// back to the 501 stub.
func (gw *GatewayConfig) mountOCIRegistry(r chi.Router) {
	r.Route("/v2", func(v2 chi.Router) {
		v2.Use(gw.hostMatch)

		// The token endpoint authenticates the presented credential in-band and
		// issues a Bearer token, so it mounts outside the bearer auth bridge.
		v2.Get("/token", gw.handleECRToken)

		v2.Group(func(reg chi.Router) {
			reg.Use(gw.ecrAuthBridge)
			reg.Get("/", gateway_ecr.APIVersion)
			if gw.ECRRegistry != nil {
				reg.HandleFunc("/*", gw.ECRRegistry.ServeHTTP)
			} else {
				reg.HandleFunc("/*", gateway_ecr.NotImplemented)
			}
		})
	})
}
