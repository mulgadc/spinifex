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
		v2.Use(gw.ecrAuthBridge)
		v2.Get("/", gateway_ecr.APIVersion)
		if gw.ECRRegistry != nil {
			v2.HandleFunc("/*", gw.ECRRegistry.ServeHTTP)
		} else {
			v2.HandleFunc("/*", gateway_ecr.NotImplemented)
		}
	})
}
