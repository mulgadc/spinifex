package gateway

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// mountRegistry mounts the registry handler at /v2, ahead of the SigV4 group. The
// registry authenticates by its own token rather than SigV4, so it is not part of the
// AWS seam. Only the exact /v2 subtree is the registry; /v20 and the like stay SigV4.
func (gw *GatewayConfig) mountRegistry(r chi.Router) {
	h := gw.RegistryHandler
	if h == nil {
		h = http.HandlerFunc(registryNotConfigured)
	}
	r.Mount("/v2", h)
}

func registryNotConfigured(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotImplemented)
	_, _ = w.Write([]byte(`{"errors":[{"code":"UNSUPPORTED","message":"registry not configured"}]}`))
}
