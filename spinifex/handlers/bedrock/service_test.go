package handlers_bedrock

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/gpu"
	"github.com/mulgadc/spinifex/spinifex/handlers/sysinstance"
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

	del, err := s.Delete(t.Context(), &DeleteEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	assert.True(t, del.Removed, "a real teardown must report Removed")

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
	assert.False(t, out.Removed, "a delete that found no record must not report Removed")
	assert.Empty(t, h.launcher.terminated)
}

func TestDelete_RefusesNonReadyState(t *testing.T) {
	h := newLaunchHarness()
	s, _ := newTestService(t, h, http.StatusOK, sufficientGPU())

	key := resolveKey(utils.GlobalAccountID, testModelID)
	rec := EndpointRecord{
		AccountID: utils.GlobalAccountID, ModelID: testModelID, State: StateStarting, Generation: 1,
	}
	_, err := s.store.Create(t.Context(), key, &rec)
	require.NoError(t, err)

	_, err = s.Delete(t.Context(), &DeleteEndpointInput{ModelID: testModelID}, "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorModelNotReadyException, awserrors.ValidErrorCodeFromError(err))
}

// TestDelete_ResumesFromDraining covers a teardown that failed partway: the
// record is already DRAINING and its VM may be gone. Without a resume the
// record is stranded, since nothing else moves it and the resolver treats
// DRAINING as neither servable nor relaunchable.
func TestDelete_ResumesFromDraining(t *testing.T) {
	h := newLaunchHarness()
	s, _ := newTestService(t, h, http.StatusOK, sufficientGPU())

	key := resolveKey(utils.GlobalAccountID, testModelID)
	rec := EndpointRecord{
		AccountID: utils.GlobalAccountID, ModelID: testModelID, State: StateDraining,
		InstanceID: "i-stranded", Generation: 2,
	}
	_, err := s.store.Create(t.Context(), key, &rec)
	require.NoError(t, err)

	_, err = s.Delete(t.Context(), &DeleteEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)

	desc, err := s.Describe(t.Context(), &DescribeEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	assert.Equal(t, StateAbsent, desc.Endpoint.State)
	assert.Contains(t, h.launcher.terminated, "i-stranded", "the resume must still tear the VM down")
}

// testAccountID is a non-Global account, distinct from utils.GlobalAccountID,
// standing in for a real tenant across the account-scoping tests below.
const testAccountID = "111111111111"

// TestEnsure_EmptyAccountIDKeysGlobalAndUnpinned pins down existing-caller
// behaviour under the widened input: an Ensure that leaves AccountID and
// Pinned at their zero values must still land under GlobalAccountID,
// unpinned, exactly as every pre-existing caller relies on.
func TestEnsure_EmptyAccountIDKeysGlobalAndUnpinned(t *testing.T) {
	h := newLaunchHarness()
	s, _ := newTestService(t, h, http.StatusOK, sufficientGPU())

	out, err := s.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	assert.Equal(t, utils.GlobalAccountID, out.Endpoint.AccountID)
	assert.False(t, out.Endpoint.Pinned)
	s.WaitLaunches()

	rec, found, err := s.store.get(t.Context(), resolveKey(utils.GlobalAccountID, testModelID))
	require.NoError(t, err)
	require.True(t, found, "an empty AccountID must key the record under GlobalAccountID")
	assert.False(t, rec.Pinned)
}

// TestEnsure_RealAccountIDKeysUnderAccountAndPersistsPinned is the PT shape:
// a real AccountID plus Pinned:true must key the record away from the shared
// GlobalAccountID endpoint and persist Pinned on it.
func TestEnsure_RealAccountIDKeysUnderAccountAndPersistsPinned(t *testing.T) {
	h := newLaunchHarness()
	s, _ := newTestService(t, h, http.StatusOK, sufficientGPU())

	out, err := s.Ensure(t.Context(), &EnsureEndpointInput{
		ModelID: testModelID, AccountID: testAccountID, Pinned: true,
	}, "")
	require.NoError(t, err)
	assert.Equal(t, testAccountID, out.Endpoint.AccountID)
	assert.True(t, out.Endpoint.Pinned)
	s.WaitLaunches()

	rec, found, err := s.store.get(t.Context(), resolveKey(testAccountID, testModelID))
	require.NoError(t, err)
	require.True(t, found, "a real AccountID must key the record under that account, not Global")
	assert.True(t, rec.Pinned)

	// Nothing must have landed under the shared Global key.
	_, foundGlobal, err := s.store.get(t.Context(), resolveKey(utils.GlobalAccountID, testModelID))
	require.NoError(t, err)
	assert.False(t, foundGlobal)
}

// TestDescribeDelete_AccountScopedRoundTrip covers Describe/Delete resolving
// the same account-scoped key an account-scoped Ensure wrote.
func TestDescribeDelete_AccountScopedRoundTrip(t *testing.T) {
	h := newLaunchHarness()
	s, _ := newTestService(t, h, http.StatusOK, sufficientGPU())

	_, err := s.Ensure(t.Context(), &EnsureEndpointInput{
		ModelID: testModelID, AccountID: testAccountID, Pinned: true,
	}, "")
	require.NoError(t, err)
	s.WaitLaunches()

	desc, err := s.Describe(t.Context(), &DescribeEndpointInput{ModelID: testModelID, AccountID: testAccountID}, "")
	require.NoError(t, err)
	assert.Equal(t, StateReady, desc.Endpoint.State)
	assert.Equal(t, testAccountID, desc.Endpoint.AccountID)
	assert.True(t, desc.Endpoint.Pinned)

	// The Global account never had an endpoint created, so it must read ABSENT.
	globalDesc, err := s.Describe(t.Context(), &DescribeEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	assert.Equal(t, StateAbsent, globalDesc.Endpoint.State)

	_, err = s.Delete(t.Context(), &DeleteEndpointInput{ModelID: testModelID, AccountID: testAccountID}, "")
	require.NoError(t, err)

	desc, err = s.Describe(t.Context(), &DescribeEndpointInput{ModelID: testModelID, AccountID: testAccountID}, "")
	require.NoError(t, err)
	assert.Equal(t, StateAbsent, desc.Endpoint.State)
}

// TestDelete_BareDeleteMissesAccountScopedPinnedButScopedClearsGoneVM is
// 6z9vg's core: a bare (Global) delete resolves a key that never held the
// pinned, account-scoped record, so it is a no-op that must report Removed=false
// and leave the record; scoped to the owning account it clears the record even
// when the VM was terminated out of band.
func TestDelete_BareDeleteMissesAccountScopedPinnedButScopedClearsGoneVM(t *testing.T) {
	h := newLaunchHarness()
	s, _ := newTestService(t, h, http.StatusOK, sufficientGPU())

	_, err := s.Ensure(t.Context(), &EnsureEndpointInput{
		ModelID: testModelID, AccountID: testAccountID, Pinned: true,
	}, "")
	require.NoError(t, err)
	s.WaitLaunches()

	bare, err := s.Delete(t.Context(), &DeleteEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	assert.False(t, bare.Removed, "a bare delete must not claim a teardown it did not do")

	desc, err := s.Describe(t.Context(), &DescribeEndpointInput{ModelID: testModelID, AccountID: testAccountID}, "")
	require.NoError(t, err)
	assert.Equal(t, StateReady, desc.Endpoint.State, "the pinned record must survive a mis-scoped delete")

	// The instance vanished out of band: the scoped delete must still clear the
	// record rather than stall on a gone VM.
	h.launcher.terminateErr = sysinstance.ErrSystemInstanceNotFound

	scoped, err := s.Delete(t.Context(), &DeleteEndpointInput{ModelID: testModelID, AccountID: testAccountID}, "")
	require.NoError(t, err)
	assert.True(t, scoped.Removed, "a scoped delete of a gone-VM record must clear it")

	desc, err = s.Describe(t.Context(), &DescribeEndpointInput{ModelID: testModelID, AccountID: testAccountID}, "")
	require.NoError(t, err)
	assert.Equal(t, StateAbsent, desc.Endpoint.State)
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

// TestList_ReturnsEndpointsAcrossAllAccountsIncludingPinned is Bug 2's core
// regression guard: a pinned, account-scoped endpoint must appear in List
// alongside the shared GlobalAccountID one, carrying its own AccountID and
// Pinned — an operator listing must not key on GlobalAccountID only.
func TestList_ReturnsEndpointsAcrossAllAccountsIncludingPinned(t *testing.T) {
	h := newLaunchHarness()
	s, _ := newTestService(t, h, http.StatusOK, sufficientGPU())

	_, err := s.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)

	pinnedModelID := "meta.llama3-2-3b-instruct-v1:0"
	_, err = s.Ensure(t.Context(), &EnsureEndpointInput{
		ModelID: pinnedModelID, AccountID: testAccountID, Pinned: true,
	}, "")
	require.NoError(t, err)
	s.WaitLaunches()

	out, err := s.List(t.Context(), &ListEndpointsInput{}, "")
	require.NoError(t, err)
	require.Len(t, out.Endpoints, 2)

	byModel := map[string]EndpointRecord{}
	for _, e := range out.Endpoints {
		byModel[e.ModelID] = e
	}
	global, ok := byModel[testModelID]
	require.True(t, ok, "the shared GlobalAccountID endpoint must still list unchanged")
	assert.Equal(t, utils.GlobalAccountID, global.AccountID)
	assert.False(t, global.Pinned)

	pinned, ok := byModel[pinnedModelID]
	require.True(t, ok, "a pinned, account-scoped endpoint must appear in List")
	assert.Equal(t, testAccountID, pinned.AccountID)
	assert.True(t, pinned.Pinned)
}

// TestEnsure_GroupMemberConvergesOnOneBundle guards the co-serve seam: a second
// member of an already-serving group must resolve to the ONE shared VM, not
// launch a second. testModelID (the vLLM primary) and the embedder below both
// resolve to coServeGroupOchreDemo, so Ensure keys them the same. Each member
// still resolves to its own port on that VM.
func TestEnsure_GroupMemberConvergesOnOneBundle(t *testing.T) {
	const embedModelID = "nomic-embed-text-v1.5"
	h := newLaunchHarness()
	s, _ := newTestService(t, h, http.StatusOK, sufficientGPU())

	_, err := s.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	s.WaitLaunches()

	out, err := s.Ensure(t.Context(), &EnsureEndpointInput{ModelID: embedModelID}, "")
	require.NoError(t, err)
	s.WaitLaunches()
	assert.EqualValues(t, 1, h.launcher.launchCount.Load(), "a group member must not launch a second VM")
	assert.Equal(t, embedModelID, out.Endpoint.ModelID, "the record mirrors the member actually asked for")

	list, err := s.List(t.Context(), &ListEndpointsInput{}, "")
	require.NoError(t, err)
	require.Len(t, list.Endpoints, 1, "the whole group is one endpoint record")

	llmDesc, err := s.Describe(t.Context(), &DescribeEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	embDesc, err := s.Describe(t.Context(), &DescribeEndpointInput{ModelID: embedModelID}, "")
	require.NoError(t, err)
	assert.Equal(t, StateReady, embDesc.Endpoint.State)

	llmURL := llmDesc.Endpoint.MemberBaseURL(testModelID)
	embURL := embDesc.Endpoint.MemberBaseURL(embedModelID)
	require.NotEmpty(t, llmURL)
	require.NotEmpty(t, embURL)
	assert.NotEqual(t, llmURL, embURL, "each member resolves to its own port on the shared VM")
	assert.Contains(t, llmURL, ":8000", "the vLLM primary keeps the well-known port")
	assert.Contains(t, embURL, ":8001", "a TEI member takes a port above the vLLM one")
}
