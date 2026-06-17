package gateway

import (
	"github.com/go-chi/chi/v5"

	gateway_ecr "github.com/mulgadc/spinifex/spinifex/gateway/ecr"
)

// mountOCIRegistry registers the OCI Distribution Spec v2 surface (/v2/*) on r.
// These endpoints are host- and token-authenticated rather than
// SigV4-credential-scoped, so they mount outside the SigV4 group under the
// no-op hostMatch skeleton.
//
// OCI repository names may contain slashes (e.g. team/app), so the blob,
// manifest and tag paths can't be expressed as fixed chi segments. Everything
// under /v2 therefore routes through a single catch-all 501 stub until a
// path-parsing dispatcher and storage exist. GET /v2/ is live so the registry
// version-check probe succeeds.
func (gw *GatewayConfig) mountOCIRegistry(r chi.Router) {
	r.Route("/v2", func(v2 chi.Router) {
		v2.Use(hostMatch)
		v2.Get("/", gateway_ecr.APIVersion)
		v2.HandleFunc("/*", gateway_ecr.NotImplemented)
	})
}
