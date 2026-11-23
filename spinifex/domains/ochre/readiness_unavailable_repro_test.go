package ochre

//test:in-package: drives the unexported launch wait and reaper sweep against
// a stub answering 503 with the package's VM, ENI and volume fakes.

import (
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func unavailableClient() *http.Client {
	return &http.Client{Transport: &stubRoundTripper{statusCode: http.StatusServiceUnavailable}}
}

// unavailableRoutes answers every request 503 and records each URL it was sent.
type unavailableRoutes struct {
	mu   sync.Mutex
	urls []string
}

func (u *unavailableRoutes) RoundTrip(req *http.Request) (*http.Response, error) {
	u.mu.Lock()
	u.urls = append(u.urls, req.URL.String())
	u.mu.Unlock()
	return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody, Header: http.Header{}, Request: req}, nil
}

func (u *unavailableRoutes) sent() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.urls)
}

func TestReadinessUnavailableRepro_LaunchTimeoutRetainsVMAndVolumes(t *testing.T) {
	t.Parallel()
	h := newLaunchHarness()
	s := newRedirectService(t, h, unavailableClient())

	_, err := s.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	s.WaitLaunches()

	rec := describeState(t, s, testModelID)
	t.Logf("state=%s instance=%q terminated=%v enisDeleted=%v volumesDeleted=%v",
		rec.State, rec.InstanceID, h.launcher.terminations(), h.vpc.deleted, h.volumes.deleted)
	assert.NotEqual(t, StateReady, rec.State, "an unavailable member must not read as ready")
	assert.Equal(t, StateStarting, rec.State, "the launch record must be retained")
	assert.NotEmpty(t, rec.InstanceID, "the retained record must name the VM it holds")
	assert.Empty(t, h.launcher.terminations(), "a startup timeout must not terminate a VM that has run")
	assert.Empty(t, h.vpc.deleted, "a startup timeout must not delete the ENI")
	assert.Empty(t, h.volumes.deleted, "a startup timeout must not delete the weights volume")
}

func TestReadinessUnavailableRepro_ReaperRepeated503RetainsVMAndVolumes(t *testing.T) {
	t.Parallel()
	probes := []struct {
		name    string
		modelID string
		path    string
	}{
		{"metrics scrape", testModelID, "/metrics"},
		{"liveness probe", "bge-base-en-v1.5", "/health"},
	}
	for _, probe := range probes {
		t.Run(probe.name, func(t *testing.T) {
			t.Parallel()
			routes := &unavailableRoutes{}
			seeded := seededReady(t, probe.modelID)
			r := newRedirectReaper(t, &http.Client{Transport: routes}, seeded)

			for range maxScrapeFailures + 1 {
				require.NoError(t, r.reaper.sweepOnce(t.Context(), nil))
			}
			r.svc.WaitLaunches()

			got, found := r.current(t)
			t.Logf("requests=%v recordFound=%v state=%s scrapeFailures=%d terminated=%v enisDeleted=%v volumesDeleted=%v relaunches=%d",
				routes.sent(), found, got.State, got.ScrapeFailures, r.harness.launcher.terminations(),
				r.harness.vpc.deleted, r.harness.volumes.deleted, r.harness.launcher.launchCount.Load())
			assert.Equal(t, slices.Repeat([]string{seeded.BaseURL + probe.path}, maxScrapeFailures+1), routes.sent(),
				"every sweep must probe the primary member's own route, and nothing else")
			assert.True(t, found, "an unavailable endpoint's record must be retained")
			assert.Equal(t, StateReady, got.State)
			assert.Equal(t, "i-redirecting", got.InstanceID, "the record must still name the VM it holds")
			assert.Empty(t, r.harness.launcher.terminations(), "repeated 503s must not terminate a VM that has run")
			assert.Empty(t, r.harness.vpc.deleted, "repeated 503s must not delete the ENI")
			assert.Empty(t, r.harness.volumes.deleted, "repeated 503s must not delete the weights volume")
			assert.Zero(t, r.harness.launcher.launchCount.Load(), "repeated 503s must not relaunch")
		})
	}
}
