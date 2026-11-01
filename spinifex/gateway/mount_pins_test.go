package gateway

//test:in-package — pins the route order of the unauthenticated mounts against
// the SigV4 group and the throttle, through the real router.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
)

func mountsApp(t *testing.T) http.Handler {
	t.Helper()
	gw := reqctxGateway(t, reqctxGatewayEnv{NATS: natsUp, Throttler: newThrottler(t)})
	gw.PublicRoutes = func(r chi.Router) {
		ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
		r.Get("/oidc/eks/{region}/{accountID}/{clusterName}/.well-known/openid-configuration", ok)
		r.Get("/oidc/eks/{region}/{accountID}/{clusterName}/keys", ok)
		r.Mount("/v2", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTeapot)
		}))
	}
	return gw.SetupRoutes()
}

// OIDC discovery and the registry sit ahead of SigV4 and the throttle: an
// unsigned caller is served, however many times, with a bucket of one.
func TestMounts_PublicRoutesPrecedeSigV4AndThrottle(t *testing.T) {
	app := mountsApp(t)
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/oidc/eks/ap-southeast-2/123456789012/c/.well-known/openid-configuration", http.StatusOK},
		{"/oidc/eks/ap-southeast-2/123456789012/c/keys", http.StatusOK},
		{"/v2/", http.StatusTeapot},
		{"/v2/team/app/tags/list", http.StatusTeapot},
	} {
		for range 4 {
			resp := doRequest(app, httptest.NewRequest(http.MethodGet, tc.path, nil))
			assert.Equal(t, tc.want, resp.StatusCode, tc.path)
		}
	}
}

// Only the exact /v2 subtree is the registry; a sibling path stays behind SigV4.
func TestMounts_RegistryPrefixIsExact(t *testing.T) {
	resp := doRequest(mountsApp(t), httptest.NewRequest(http.MethodGet, "/v20/x", nil))
	assert.NotEqual(t, http.StatusTeapot, resp.StatusCode)
	assert.GreaterOrEqual(t, resp.StatusCode, http.StatusBadRequest)
}

// Unsigned /admin is refused by SigV4, so the mount cannot be reached without it.
func TestMounts_AdminSitsBehindSigV4(t *testing.T) {
	resp := doRequest(mountsApp(t), httptest.NewRequest(http.MethodPost, "/admin/CreateAccount", nil))
	assert.GreaterOrEqual(t, resp.StatusCode, http.StatusBadRequest)
}
