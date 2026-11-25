package ochre

import (
	"net/http"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newPeerService is a second daemon over the same NATS cluster as a service
// built by newTestService.
func newPeerService(nc *nats.Conn, h *launchHarness) *Service {
	return NewService(nc, ServiceDeps{
		Config:         &config.Config{Region: "us-east-1", AZ: "us-east-1a"},
		Launch:         h.deps(),
		GPU:            sufficientGPU(),
		NodeID:         "node-2",
		StartupTimeout: 200 * time.Millisecond,
		HTTPClient:     &http.Client{Transport: &stubRoundTripper{statusCode: http.StatusOK}},
		PollInterval:   5 * time.Millisecond,
		Replicas:       1,
	})
}

// Two daemons resume one stranded teardown. The peer finishes first and an
// operator pins a new endpoint on the key; the slower daemon's teardown must
// not remove that replacement's record.
func TestDelete_SlowTeardownLeavesReplacementPinnedEndpoint(t *testing.T) {
	t.Parallel()
	slow := newLaunchHarness()
	s, nc := newTestService(t, slow, http.StatusOK, sufficientGPU())
	fast := newLaunchHarness()
	peer := newPeerService(nc, fast)

	key := resolveKey(awsidentifiers.GlobalAccountID, testModelID)
	stranded := EndpointRecord{
		AccountID: awsidentifiers.GlobalAccountID, ModelID: testModelID, State: StateDraining,
		InstanceID: "i-stranded", Generation: 2,
	}
	_, err := s.store.Create(t.Context(), key, &stranded)
	require.NoError(t, err)

	slow.launcher.onTerminate = func(string) {
		out, err := peer.Delete(t.Context(), &DeleteEndpointInput{ModelID: testModelID}, "")
		require.NoError(t, err)
		require.True(t, out.Removed)
		_, err = peer.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID, Pinned: true, OperatorReserved: true}, "")
		require.NoError(t, err)
		peer.WaitLaunches()
	}

	out, err := s.Delete(t.Context(), &DeleteEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	assert.True(t, out.Removed, "the slow daemon still tore its own VM down")
	assert.Contains(t, slow.launcher.terminations(), "i-stranded")

	desc, err := s.Describe(t.Context(), &DescribeEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	assert.Equal(t, StateReady, desc.Endpoint.State, "the replacement's record survives the old teardown")
	assert.True(t, desc.Endpoint.Pinned)
	assert.NotEqual(t, "i-stranded", desc.Endpoint.InstanceID)
	assert.Equal(t, []string{"i-stranded"}, fast.launcher.terminations(), "nothing tore the replacement down")
}

// A guarded removal leaves a delete marker, and an ordinary Ensure afterwards
// still claims the key and launches.
func TestDelete_RecreateAfterGuardedRemoval(t *testing.T) {
	t.Parallel()
	h := newLaunchHarness()
	s, _ := newTestService(t, h, http.StatusOK, sufficientGPU())

	_, err := s.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	s.WaitLaunches()
	_, err = s.Delete(t.Context(), &DeleteEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)

	out, err := s.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	assert.Equal(t, StateStarting, out.Endpoint.State)
	s.WaitLaunches()

	desc, err := s.Describe(t.Context(), &DescribeEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	assert.Equal(t, StateReady, desc.Endpoint.State)
	assert.EqualValues(t, 2, h.launcher.launchCount.Load())
}
