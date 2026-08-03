package handlers_bedrock

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/gpu"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sufficientGPU is a GPU snapshot with plenty of room for testModelID's
// 5120 MiB MinVRAMMiB floor.
func sufficientGPU() *stubSnapshotter {
	return &stubSnapshotter{entries: []gpu.PoolEntry{{Device: gpu.GPUDevice{MemoryMiB: 8192}, Available: true}}}
}

// newTestService wires a Service against an embedded JetStream server and h's
// fakes, with a short startup timeout/poll interval so readiness tests never
// wait out anything close to production timing. statusCode drives every
// readiness probe response via a stubRoundTripper.
func newTestService(t *testing.T, h *launchHarness, statusCode int32, gpuSnap gpuSnapshotter) (*Service, *nats.Conn) {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	deps := ServiceDeps{
		Config:         &config.Config{Region: "us-east-1", AZ: "us-east-1a"},
		Launch:         h.deps(),
		GPU:            gpuSnap,
		NodeID:         "node-1",
		StartupTimeout: 200 * time.Millisecond,
		HTTPClient:     &http.Client{Transport: &stubRoundTripper{statusCode: statusCode}},
		PollInterval:   5 * time.Millisecond,
		Replicas:       1,
	}
	return NewService(nc, deps), nc
}

func TestEnsure_ConcurrentSameProcessLaunchesExactlyOneVM(t *testing.T) {
	h := newLaunchHarness()
	s, _ := newTestService(t, h, http.StatusOK, sufficientGPU())

	const n = 20
	var wg sync.WaitGroup
	outs := make([]*EnsureEndpointOutput, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, err := s.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")
			outs[i], errs[i] = out, err
		}(i)
	}
	wg.Wait()
	s.WaitLaunches()

	for i := range n {
		require.NoError(t, errs[i])
		require.NotNil(t, outs[i])
	}
	assert.EqualValues(t, 1, h.launcher.launchCount.Load())

	desc, err := s.Describe(t.Context(), &DescribeEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	assert.Equal(t, StateReady, desc.Endpoint.State)
}

// TestEnsure_CrossReplicaCASCollapsesToOneLaunch exercises the layer the
// per-process ensureMu cannot: two independent Service instances (standing in
// for two daemon replicas) sharing one JetStream backend. Only the KV
// Create/CAS in store.go decides the winner here.
func TestEnsure_CrossReplicaCASCollapsesToOneLaunch(t *testing.T) {
	h := newLaunchHarness()
	_, nc, _ := testutil.StartTestJetStream(t)
	newDeps := func() ServiceDeps {
		return ServiceDeps{
			Launch:         h.deps(),
			GPU:            sufficientGPU(),
			StartupTimeout: 200 * time.Millisecond,
			HTTPClient:     &http.Client{Transport: &stubRoundTripper{statusCode: http.StatusOK}},
			PollInterval:   5 * time.Millisecond,
			Replicas:       1,
		}
	}
	s1 := NewService(nc, newDeps())
	s2 := NewService(nc, newDeps())

	var wg sync.WaitGroup
	var err1, err2 error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err1 = s1.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")
	}()
	go func() {
		defer wg.Done()
		_, err2 = s2.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")
	}()
	wg.Wait()
	s1.WaitLaunches()
	s2.WaitLaunches()

	require.NoError(t, err1)
	require.NoError(t, err2)
	assert.EqualValues(t, 1, h.launcher.launchCount.Load())
}

func TestEnsure_CapacityRefusal(t *testing.T) {
	h := newLaunchHarness()
	s, _ := newTestService(t, h, http.StatusOK, &stubSnapshotter{})

	_, err := s.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorModelNotReadyException, awserrors.ValidErrorCodeFromError(err))
	assert.Zero(t, h.launcher.launchCount.Load())

	desc, err := s.Describe(t.Context(), &DescribeEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	assert.Equal(t, StateAbsent, desc.Endpoint.State, "a refused admission must never leave a STARTING record")
}

func TestEnsure_UnknownModelRejected(t *testing.T) {
	h := newLaunchHarness()
	s, _ := newTestService(t, h, http.StatusOK, sufficientGPU())

	_, err := s.Ensure(t.Context(), &EnsureEndpointInput{ModelID: "no-such-model"}, "")
	require.Error(t, err)
	assert.Zero(t, h.launcher.launchCount.Load())
}

func TestEnsure_ReachesReady(t *testing.T) {
	h := newLaunchHarness()
	s, _ := newTestService(t, h, http.StatusOK, sufficientGPU())

	out, err := s.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	assert.Equal(t, StateStarting, out.Endpoint.State)

	s.WaitLaunches()

	desc, err := s.Describe(t.Context(), &DescribeEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	assert.Equal(t, StateReady, desc.Endpoint.State)
	assert.NotEmpty(t, desc.Endpoint.BaseURL)
	assert.False(t, desc.Endpoint.ReadyAt.IsZero())
	assert.EqualValues(t, 2, desc.Endpoint.Generation)
}

func TestEnsure_ReadinessTimeoutAbortsToAbsent(t *testing.T) {
	h := newLaunchHarness()
	s, _ := newTestService(t, h, http.StatusServiceUnavailable, sufficientGPU())

	_, err := s.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	s.WaitLaunches()

	desc, err := s.Describe(t.Context(), &DescribeEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	assert.Equal(t, StateAbsent, desc.Endpoint.State, "a launch that never becomes ready must revert, not stick STARTING")

	assert.NotEmpty(t, h.launcher.terminated, "the VM that never became ready must be unwound")
	assert.NotEmpty(t, h.vpc.deleted)
	assert.NotEmpty(t, h.volumes.deleted)
}

func TestDelete_ReadyToAbsent(t *testing.T) {
	h := newLaunchHarness()
	s, _ := newTestService(t, h, http.StatusOK, sufficientGPU())

	_, err := s.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	s.WaitLaunches()

	desc, err := s.Describe(t.Context(), &DescribeEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	require.Equal(t, StateReady, desc.Endpoint.State)
	instanceID := desc.Endpoint.InstanceID

	_, err = s.Delete(t.Context(), &DeleteEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)

	desc, err = s.Describe(t.Context(), &DescribeEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	assert.Equal(t, StateAbsent, desc.Endpoint.State)
	assert.Contains(t, h.launcher.terminated, instanceID)
}

func TestDelete_IdempotentOnAbsent(t *testing.T) {
	h := newLaunchHarness()
	s, _ := newTestService(t, h, http.StatusOK, sufficientGPU())

	out, err := s.Delete(t.Context(), &DeleteEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	assert.NotNil(t, out)
	assert.Empty(t, h.launcher.terminated)
}

func TestDelete_RefusesNonReadyState(t *testing.T) {
	h := newLaunchHarness()
	s, nc := newTestService(t, h, http.StatusOK, sufficientGPU())

	js := testutil.NewJetStream(t, nc)
	kv, err := GetOrCreateEndpointsBucket(t.Context(), js, 1)
	require.NoError(t, err)
	key := EndpointKey(utils.GlobalAccountID, testModelID)
	_, err = createJSONRevision(t.Context(), kv, key, EndpointRecord{
		AccountID: utils.GlobalAccountID, ModelID: testModelID, State: StateStarting, Generation: 1,
	})
	require.NoError(t, err)

	_, err = s.Delete(t.Context(), &DeleteEndpointInput{ModelID: testModelID}, "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorModelNotReadyException, awserrors.ValidErrorCodeFromError(err))
}

func TestList_ReturnsAllEnsuredEndpoints(t *testing.T) {
	h := newLaunchHarness()
	s, _ := newTestService(t, h, http.StatusOK, sufficientGPU())

	_, err := s.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	s.WaitLaunches()

	out, err := s.List(t.Context(), &ListEndpointsInput{}, "")
	require.NoError(t, err)
	require.Len(t, out.Endpoints, 1)
	assert.Equal(t, testModelID, out.Endpoints[0].ModelID)
}
