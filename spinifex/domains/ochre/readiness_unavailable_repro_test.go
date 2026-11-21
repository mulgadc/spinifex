package ochre

//test:in-package: drives the unexported launch wait and reaper sweep against
// a stub answering 503 with the package's VM, ENI and volume fakes.

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func unavailableClient() *http.Client {
	return &http.Client{Transport: &stubRoundTripper{statusCode: http.StatusServiceUnavailable}}
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
		family  string
		port    int
	}{
		{"metrics scrape", testModelID, FamilyMeta, vllmServePort},
		{"liveness probe", "embed-only-bundle", FamilyTEI, teiBasePort},
	}
	for _, probe := range probes {
		t.Run(probe.name, func(t *testing.T) {
			t.Parallel()
			r := newRedirectReaper(t, unavailableClient(), seededReady(probe.modelID, probe.family, probe.port))

			for range maxScrapeFailures + 1 {
				require.NoError(t, r.reaper.sweepOnce(t.Context(), nil))
			}
			r.svc.WaitLaunches()

			got, found := r.current(t)
			t.Logf("recordFound=%v state=%s scrapeFailures=%d terminated=%v enisDeleted=%v volumesDeleted=%v relaunches=%d",
				found, got.State, got.ScrapeFailures, r.harness.launcher.terminations(),
				r.harness.vpc.deleted, r.harness.volumes.deleted, r.harness.launcher.launchCount.Load())
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
