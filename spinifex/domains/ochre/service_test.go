package ochre

import (
	"context"
	stderrors "errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/systeminstance"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/gpu"
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	h := newLaunchHarness()
	s, _ := newTestService(t, h, http.StatusOK, sufficientGPU())

	_, err := s.Ensure(t.Context(), &EnsureEndpointInput{ModelID: "no-such-model"}, "")
	require.Error(t, err)
	assert.Zero(t, h.launcher.launchCount.Load())
}

func TestEnsure_ReachesReady(t *testing.T) {
	t.Parallel()
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
	assert.EqualValues(t, 3, desc.Endpoint.Generation, "claim, identity and READY are one write each")
}

// probeSnapshot answers every readiness probe with 200 and records the stored
// endpoint as the first probe saw it.
type probeSnapshot struct {
	t    *testing.T
	s    *Service
	once sync.Once
	rec  EndpointRecord
	ok   bool
}

func (p *probeSnapshot) RoundTrip(*http.Request) (*http.Response, error) {
	p.once.Do(func() {
		rec, _, found, err := p.s.store.getRevision(context.Background(), resolveKey(awsidentifiers.GlobalAccountID, testModelID))
		assert.NoError(p.t, err)
		p.rec, p.ok = rec, found
	})
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
}

func TestEnsure_IdentityIsRecordedBeforeTheProbe(t *testing.T) {
	t.Parallel()
	h := newLaunchHarness()
	s, _ := newTestService(t, h, http.StatusOK, sufficientGPU())
	probe := &probeSnapshot{t: t, s: s}
	s.deps.HTTPClient = &http.Client{Transport: probe}

	_, err := s.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	s.WaitLaunches()

	require.True(t, probe.ok, "the claim must exist when the probe runs")
	seen := probe.rec
	assert.Equal(t, StateStarting, seen.State)
	assert.Equal(t, "i-1", seen.InstanceID)
	assert.NotEmpty(t, seen.ENIID)
	assert.NotEmpty(t, seen.PrivateIP)
	require.Contains(t, seen.Members, testModelID)
	assert.Equal(t, vllmServePort, seen.Members[testModelID].Port)
	assert.NotEmpty(t, seen.Members[testModelID].WeightsVolumeID)
	assert.Equal(t, seen.Members[testModelID].WeightsVolumeID, seen.WeightsVolumeID)
	assert.Empty(t, seen.BaseURL, "the serving address is published with READY, not before")

	got, found := storedEndpoint(t, s)
	require.True(t, found)
	assert.Equal(t, StateReady, got.State)
	assert.Equal(t, seen.InstanceID, got.InstanceID)
	assert.Equal(t, seen.Members, got.Members)
	assert.NotEmpty(t, got.BaseURL)
}

// assertRetained checks that nothing the launch realized was torn down.
func assertRetained(t *testing.T, h *launchHarness) {
	t.Helper()
	assert.Empty(t, h.launcher.terminations(), "the launched VM must not be terminated")
	assert.Empty(t, h.vpc.deleted, "the launched ENI must not be deleted")
	assert.Empty(t, h.volumes.deleted, "the weights volume must not be deleted")
}

func TestEnsure_ReadinessTimeoutRetainsTheLaunch(t *testing.T) {
	t.Parallel()
	h := newLaunchHarness()
	s, _ := newTestService(t, h, http.StatusServiceUnavailable, sufficientGPU())

	_, err := s.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	s.WaitLaunches()

	got, found := storedEndpoint(t, s)
	require.True(t, found, "a launch that never became ready keeps its claim")
	assert.Equal(t, StateStarting, got.State)
	assert.Equal(t, "i-1", got.InstanceID, "the retained claim names the VM it holds")
	assert.NotEmpty(t, got.ENIID)
	assert.NotEmpty(t, got.WeightsVolumeID)
	assertRetained(t, h)
}

func TestDelete_ReadyToAbsent(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	h := newLaunchHarness()
	s, _ := newTestService(t, h, http.StatusOK, sufficientGPU())

	out, err := s.Delete(t.Context(), &DeleteEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	assert.NotNil(t, out)
	assert.False(t, out.Removed, "a delete that found no record must not report Removed")
	assert.Empty(t, h.launcher.terminated)
}

func TestDelete_RefusesNonReadyState(t *testing.T) {
	t.Parallel()
	h := newLaunchHarness()
	s, _ := newTestService(t, h, http.StatusOK, sufficientGPU())

	key := resolveKey(awsidentifiers.GlobalAccountID, testModelID)
	rec := EndpointRecord{
		AccountID: awsidentifiers.GlobalAccountID, ModelID: testModelID, State: StateStarting, Generation: 1,
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
	t.Parallel()
	h := newLaunchHarness()
	s, _ := newTestService(t, h, http.StatusOK, sufficientGPU())

	key := resolveKey(awsidentifiers.GlobalAccountID, testModelID)
	rec := EndpointRecord{
		AccountID: awsidentifiers.GlobalAccountID, ModelID: testModelID, State: StateDraining,
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

// testAccountID is a non-Global account, distinct from awsidentifiers.GlobalAccountID,
// standing in for a real tenant across the account-scoping tests below.
const testAccountID = "111111111111"

// TestEnsure_EmptyAccountIDKeysGlobalAndUnpinned pins down existing-caller
// behaviour under the widened input: an Ensure that leaves AccountID and
// Pinned at their zero values must still land under GlobalAccountID,
// unpinned, exactly as every pre-existing caller relies on.
func TestEnsure_EmptyAccountIDKeysGlobalAndUnpinned(t *testing.T) {
	t.Parallel()
	h := newLaunchHarness()
	s, _ := newTestService(t, h, http.StatusOK, sufficientGPU())

	out, err := s.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	assert.Equal(t, awsidentifiers.GlobalAccountID, out.Endpoint.AccountID)
	assert.False(t, out.Endpoint.Pinned)
	s.WaitLaunches()

	rec, found, err := s.store.get(t.Context(), resolveKey(awsidentifiers.GlobalAccountID, testModelID))
	require.NoError(t, err)
	require.True(t, found, "an empty AccountID must key the record under GlobalAccountID")
	assert.False(t, rec.Pinned)
}

// TestEnsure_RealAccountIDKeysUnderAccountAndPersistsPinned is the PT shape:
// a real AccountID plus Pinned:true must key the record away from the shared
// GlobalAccountID endpoint and persist Pinned on it.
func TestEnsure_RealAccountIDKeysUnderAccountAndPersistsPinned(t *testing.T) {
	t.Parallel()
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
	_, foundGlobal, err := s.store.get(t.Context(), resolveKey(awsidentifiers.GlobalAccountID, testModelID))
	require.NoError(t, err)
	assert.False(t, foundGlobal)
}

// TestDescribeDelete_AccountScopedRoundTrip covers Describe/Delete resolving
// the same account-scoped key an account-scoped Ensure wrote.
func TestDescribeDelete_AccountScopedRoundTrip(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	h.launcher.terminateErr = systeminstance.ErrSystemInstanceNotFound

	scoped, err := s.Delete(t.Context(), &DeleteEndpointInput{ModelID: testModelID, AccountID: testAccountID}, "")
	require.NoError(t, err)
	assert.True(t, scoped.Removed, "a scoped delete of a gone-VM record must clear it")

	desc, err = s.Describe(t.Context(), &DescribeEndpointInput{ModelID: testModelID, AccountID: testAccountID}, "")
	require.NoError(t, err)
	assert.Equal(t, StateAbsent, desc.Endpoint.State)
}

func TestList_ReturnsAllEnsuredEndpoints(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	assert.Equal(t, awsidentifiers.GlobalAccountID, global.AccountID)
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
	t.Parallel()
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

// changeClaim rewrites the endpoint record as another writer would, through a
// CAS on its current revision. It runs on the launch worker, so it reports
// failures with assert rather than stopping the test goroutine.
func changeClaim(t *testing.T, s *Service) EndpointRecord {
	t.Helper()
	key := resolveKey(awsidentifiers.GlobalAccountID, testModelID)
	rec, rev, found, err := s.store.getRevision(context.Background(), key)
	if !assert.NoError(t, err) || !assert.True(t, found, "the claim must exist while its launch runs") {
		return EndpointRecord{}
	}
	rec.NodeID = "node-other"
	rec.CreatedAt = rec.CreatedAt.Add(time.Second)
	_, err = s.store.CompareAndSet(context.Background(), key, &rec, rev)
	assert.NoError(t, err)
	return rec
}

func storedEndpoint(t *testing.T, s *Service) (EndpointRecord, bool) {
	t.Helper()
	rec, _, found, err := s.store.getRevision(t.Context(), resolveKey(awsidentifiers.GlobalAccountID, testModelID))
	require.NoError(t, err)
	return rec, found
}

func TestEnsure_ChangedClaimBeforeIdentityIsNotOverwritten(t *testing.T) {
	t.Parallel()
	h := newLaunchHarness()
	s, _ := newTestService(t, h, http.StatusOK, sufficientGPU())
	var changed EndpointRecord
	h.launcher.onLaunch = func() { changed = changeClaim(t, s) }

	_, err := s.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	s.WaitLaunches()

	got, found := storedEndpoint(t, s)
	require.True(t, found, "a changed claim must not be removed")
	assert.Equal(t, StateStarting, got.State, "READY must not be published over a changed claim")
	assert.Equal(t, "node-other", got.NodeID)
	assert.True(t, got.CreatedAt.Equal(changed.CreatedAt))
	assert.Empty(t, got.InstanceID, "this launch's instance must not be written into another claim")
	assertRetained(t, h)
}

// changingProbe changes the claim during the first readiness probe, after the
// identity write, then answers 200.
type changingProbe struct {
	t       *testing.T
	s       *Service
	code    int
	once    sync.Once
	changed EndpointRecord
}

func (c *changingProbe) RoundTrip(*http.Request) (*http.Response, error) {
	c.once.Do(func() { c.changed = changeClaim(c.t, c.s) })
	code := c.code
	if code == 0 {
		code = http.StatusOK
	}
	return &http.Response{StatusCode: code, Body: http.NoBody, Header: make(http.Header)}, nil
}

func TestEnsure_ChangedClaimDuringProbeIsNotOverwrittenByReady(t *testing.T) {
	t.Parallel()
	h := newLaunchHarness()
	s, _ := newTestService(t, h, http.StatusOK, sufficientGPU())
	probe := &changingProbe{t: t, s: s}
	s.deps.HTTPClient = &http.Client{Transport: probe}

	_, err := s.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	s.WaitLaunches()

	require.Equal(t, "i-1", probe.changed.InstanceID, "the change must land after the identity write")
	got, found := storedEndpoint(t, s)
	require.True(t, found)
	assert.Equal(t, probe.changed, got, "READY must leave the newer record unchanged")
	assert.Equal(t, StateStarting, got.State)
	assert.Empty(t, got.BaseURL)
	assertRetained(t, h)
}

func TestEnsure_LaunchFailureAbortIsGuardedOnTheClaim(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change bool
	}{
		{name: "own claim is removed", change: false},
		{name: "changed claim stays", change: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newLaunchHarness()
			s, _ := newTestService(t, h, http.StatusOK, sufficientGPU())
			h.launcher.failLaunch = stderrors.New("launch refused")
			if tc.change {
				h.launcher.onLaunch = func() { changeClaim(t, s) }
			}

			_, err := s.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")
			require.NoError(t, err)
			s.WaitLaunches()

			got, found := storedEndpoint(t, s)
			if !tc.change {
				assert.False(t, found, "a failed launch must revert its own claim")
				return
			}
			require.True(t, found, "an abort must not remove a claim it no longer holds")
			assert.Equal(t, "node-other", got.NodeID)
		})
	}
}

func TestEnsure_ChangedClaimAtIdentityWriteRetainsTheLaunch(t *testing.T) {
	t.Parallel()
	h := newLaunchHarness()
	s, _ := newTestService(t, h, http.StatusServiceUnavailable, sufficientGPU())
	h.launcher.onLaunch = func() { changeClaim(t, s) }

	_, err := s.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	s.WaitLaunches()

	got, found := storedEndpoint(t, s)
	require.True(t, found, "a conflicting identity write must not remove the changed claim")
	assert.Equal(t, "node-other", got.NodeID)
	assert.Empty(t, got.InstanceID, "the launched VM must not be written into another claim")
	assertRetained(t, h)
}

func TestEnsure_UncertainIdentityWriteRetainsTheLaunch(t *testing.T) {
	t.Parallel()
	errLost := stderrors.New("nats: timeout")
	for _, tc := range []struct {
		name   string
		landed bool
	}{
		{name: "write landed and its reply was lost", landed: true},
		{name: "write never landed", landed: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newLaunchHarness()
			s, _ := newTestService(t, h, http.StatusOK, sufficientGPU())
			s.identityWrite = func(write func() (uint64, error)) (uint64, error) {
				if tc.landed {
					_, err := write()
					assert.NoError(t, err)
				}
				return 0, errLost
			}

			_, err := s.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")
			require.NoError(t, err)
			s.WaitLaunches()

			got, found := storedEndpoint(t, s)
			require.True(t, found, "an uncertain identity write must not delete the claim")
			assert.Equal(t, StateStarting, got.State, "READY must not follow an uncertain identity write")
			if tc.landed {
				assert.Equal(t, "i-1", got.InstanceID)
			} else {
				assert.Empty(t, got.InstanceID)
			}
			assertRetained(t, h)
		})
	}
}

// countingProbe answers every readiness probe with 200 and counts them.
type countingProbe struct{ hits atomic.Int32 }

func (c *countingProbe) RoundTrip(*http.Request) (*http.Response, error) {
	c.hits.Add(1)
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
}

func TestEnsure_IdentityWriteLandingAfterItsTimeoutIsNotActedOn(t *testing.T) {
	t.Parallel()
	h := newLaunchHarness()
	s, _ := newTestService(t, h, http.StatusOK, sufficientGPU())
	probe := &countingProbe{}
	s.deps.HTTPClient = &http.Client{Transport: probe}
	release, landed := make(chan struct{}), make(chan error, 1)
	s.identityWrite = func(write func() (uint64, error)) (uint64, error) {
		go func() {
			<-release
			_, err := write()
			landed <- err
		}()
		return 0, stderrors.New("nats: timeout")
	}

	_, err := s.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	s.WaitLaunches()

	before, found := storedEndpoint(t, s)
	require.True(t, found)
	require.Empty(t, before.InstanceID, "the in-flight write must not have landed before the worker's read")

	close(release)
	require.NoError(t, <-landed, "the delayed write lands at the claim revision")

	got, found := storedEndpoint(t, s)
	require.True(t, found, "the claim must not be deleted")
	assert.Equal(t, StateStarting, got.State, "READY must not be published")
	assert.Equal(t, "i-1", got.InstanceID)
	assert.Zero(t, probe.hits.Load(), "the worker must not probe after an uncertain identity write")
	assertRetained(t, h)
}

func TestEnsure_PanicAfterLaunchKeepsTheClaim(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		written bool
	}{
		{name: "after the identity write", written: true},
		{name: "before the identity write", written: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newLaunchHarness()
			s, _ := newTestService(t, h, http.StatusOK, sufficientGPU())
			s.identityWrite = func(write func() (uint64, error)) (uint64, error) {
				if tc.written {
					_, err := write()
					assert.NoError(t, err)
				}
				panic("worker exploded")
			}

			_, err := s.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")
			require.NoError(t, err)
			s.WaitLaunches()

			got, found := storedEndpoint(t, s)
			require.True(t, found, "a panic after the VM was returned must keep its claim")
			assert.Equal(t, StateStarting, got.State)
			if tc.written {
				assert.Equal(t, "i-1", got.InstanceID)
			}
			assertRetained(t, h)
		})
	}
}

func TestEnsure_PanicBeforeLaunchReturnsAbortIsGuardedOnTheClaim(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change bool
	}{
		{name: "own claim is removed", change: false},
		{name: "changed claim stays", change: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newLaunchHarness()
			s, _ := newTestService(t, h, http.StatusOK, sufficientGPU())
			h.launcher.onLaunch = func() {
				if tc.change {
					changeClaim(t, s)
				}
				panic("launcher exploded")
			}

			_, err := s.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")
			require.NoError(t, err)
			s.WaitLaunches()

			got, found := storedEndpoint(t, s)
			if !tc.change {
				assert.False(t, found, "a panicked launch must revert its own claim")
				return
			}
			require.True(t, found, "a panicked launch must not remove a claim it no longer holds")
			assert.Equal(t, "node-other", got.NodeID)
		})
	}
}

func TestEnsure_NewClaimAfterGuardedAbortReachesReady(t *testing.T) {
	t.Parallel()
	h := newLaunchHarness()
	s, _ := newTestService(t, h, http.StatusOK, sufficientGPU())
	h.launcher.failLaunch = stderrors.New("launch refused")

	_, err := s.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	s.WaitLaunches()
	_, found := storedEndpoint(t, s)
	require.False(t, found)

	h.launcher.failLaunch = nil
	out, err := s.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	assert.Equal(t, StateStarting, out.Endpoint.State)
	s.WaitLaunches()

	got, found := storedEndpoint(t, s)
	require.True(t, found)
	assert.Equal(t, StateReady, got.State, "a claim over the guarded delete's tombstone must publish READY")
	assert.EqualValues(t, 2, h.launcher.launchCount.Load())
}

// memberProbe answers each member's readiness probe by port, so a test can
// give each member of a bundle its own behaviour. It counts requests per host.
type memberProbe struct {
	mu     sync.Mutex
	hits   map[string]int
	answer func(port string, n int) *http.Response
}

func (m *memberProbe) RoundTrip(r *http.Request) (*http.Response, error) {
	m.mu.Lock()
	if m.hits == nil {
		m.hits = map[string]int{}
	}
	m.hits[r.URL.Host]++
	n := m.hits[r.URL.Host]
	m.mu.Unlock()
	return m.answer(r.URL.Port(), n), nil
}

func (m *memberProbe) count(host string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hits[host]
}

func probeAnswer(code int, location string) *http.Response {
	resp := &http.Response{StatusCode: code, Body: http.NoBody, Header: make(http.Header)}
	if location != "" {
		resp.Header.Set("Location", location)
	}
	return resp
}

const redirectTarget = "redirect-target.invalid"

// waitLaunchesWithin fails the test if the launch worker outlives limit; it
// guards a wait that must end on a redirect long before the startup timeout.
func waitLaunchesWithin(t *testing.T, s *Service, limit time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() { s.WaitLaunches(); close(done) }()
	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("launch worker still running after %s", limit)
	}
}

func TestEnsure_RedirectingMemberRetainsTheLaunchAndCancelsTheOthers(t *testing.T) {
	t.Parallel()
	for name, client := range map[string]func(*memberProbe) *http.Client{
		"following client": func(p *memberProbe) *http.Client { return &http.Client{Transport: p} },
		"no-follow client": func(p *memberProbe) *http.Client {
			return &http.Client{Transport: p, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newLaunchHarness()
			s, _ := newTestService(t, h, http.StatusOK, sufficientGPU())
			probe := &memberProbe{answer: func(port string, _ int) *http.Response {
				if port == strconv.Itoa(vllmServePort) {
					return probeAnswer(http.StatusTemporaryRedirect, "http://"+redirectTarget+"/v1/models")
				}
				return probeAnswer(http.StatusServiceUnavailable, "")
			}}
			s.deps.HTTPClient = client(probe)
			s.deps.StartupTimeout = time.Hour

			_, err := s.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")
			require.NoError(t, err)
			waitLaunchesWithin(t, s, 30*time.Second)

			got, found := storedEndpoint(t, s)
			require.True(t, found, "a redirect must keep the claim")
			assert.Equal(t, StateStarting, got.State, "a redirecting member must not read as ready")
			assert.Equal(t, "i-1", got.InstanceID)
			assert.Greater(t, len(got.Members), 1, "the bundle's every member identity is retained")
			assert.Empty(t, got.BaseURL)
			assert.Zero(t, probe.count(redirectTarget), "the redirect target must receive no probe")
			assertRetained(t, h)
		})
	}
}

func TestEnsure_UnavailableThenReadyReachesReady(t *testing.T) {
	t.Parallel()
	h := newLaunchHarness()
	s, _ := newTestService(t, h, http.StatusOK, sufficientGPU())
	probe := &memberProbe{answer: func(_ string, n int) *http.Response {
		if n <= 2 {
			return probeAnswer(http.StatusServiceUnavailable, "")
		}
		return probeAnswer(http.StatusOK, "")
	}}
	s.deps.HTTPClient = &http.Client{Transport: probe}
	s.deps.StartupTimeout = time.Hour

	_, err := s.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	waitLaunchesWithin(t, s, 30*time.Second)

	got, found := storedEndpoint(t, s)
	require.True(t, found)
	assert.Equal(t, StateReady, got.State)
	for modelID, m := range got.Members {
		assert.Equal(t, 3, probe.count(net.JoinHostPort(got.PrivateIP, strconv.Itoa(m.Port))), modelID)
	}
	assertRetained(t, h)
}

// assertParkedWithIdentity checks a parked record still carries every
// identity the launch realized.
func assertParkedWithIdentity(t *testing.T, got EndpointRecord, reason ParkReason) {
	t.Helper()
	assert.Equal(t, StateStarting, got.State)
	assert.True(t, got.Parked())
	assert.Equal(t, reason, got.ParkedReason)
	assert.False(t, got.ParkedAt.IsZero())
	assert.Equal(t, "i-1", got.InstanceID)
	assert.NotEmpty(t, got.ENIID)
	assert.NotEmpty(t, got.PrivateIP)
	assert.NotEmpty(t, got.WeightsVolumeID)
	assert.Empty(t, got.BaseURL)
	assert.Len(t, got.Members, 3, "every bundle member's identity is retained")
	for modelID, m := range got.Members {
		assert.NotZero(t, m.Port, modelID)
		assert.NotEmpty(t, m.WeightsVolumeID, modelID)
	}
	assert.EqualValues(t, 3, got.Generation, "claim, identity and park are one write each")
}

func TestEnsure_UnreadyLaunchIsParkedWithItsReason(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		probe  func(port string, n int) *http.Response
		wait   time.Duration
		reason ParkReason
	}{
		{
			name: "redirect",
			probe: func(port string, _ int) *http.Response {
				if port == strconv.Itoa(vllmServePort) {
					return probeAnswer(http.StatusFound, "http://"+redirectTarget+"/v1/models")
				}
				return probeAnswer(http.StatusServiceUnavailable, "")
			},
			wait:   time.Hour,
			reason: ParkReadinessRedirected,
		},
		{
			name:   "timeout",
			probe:  func(string, int) *http.Response { return probeAnswer(http.StatusServiceUnavailable, "") },
			wait:   50 * time.Millisecond,
			reason: ParkReadinessTimedOut,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newLaunchHarness()
			s, _ := newTestService(t, h, http.StatusOK, sufficientGPU())
			probe := &memberProbe{answer: tc.probe}
			s.deps.HTTPClient = &http.Client{Transport: probe}
			s.deps.StartupTimeout = tc.wait

			_, err := s.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")
			require.NoError(t, err)
			waitLaunchesWithin(t, s, 30*time.Second)

			got, found := storedEndpoint(t, s)
			require.True(t, found)
			assertParkedWithIdentity(t, got, tc.reason)
			assert.Zero(t, probe.count(redirectTarget))
			assertRetained(t, h)

			desc, err := s.Describe(t.Context(), &DescribeEndpointInput{ModelID: testModelID}, "")
			require.NoError(t, err)
			assert.Equal(t, tc.reason, desc.Endpoint.ParkedReason, "the reason is visible to a describe")
		})
	}
}

func TestEnsure_ChangedClaimBeforeParkIsNotParked(t *testing.T) {
	t.Parallel()
	h := newLaunchHarness()
	s, _ := newTestService(t, h, http.StatusOK, sufficientGPU())
	probe := &changingProbe{t: t, s: s, code: http.StatusServiceUnavailable}
	s.deps.HTTPClient = &http.Client{Transport: probe}
	s.deps.StartupTimeout = 50 * time.Millisecond

	_, err := s.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	s.WaitLaunches()

	require.Equal(t, "i-1", probe.changed.InstanceID, "the change must land after the identity write")
	got, found := storedEndpoint(t, s)
	require.True(t, found)
	assert.Equal(t, probe.changed, got, "the park must leave the newer record unchanged")
	assert.False(t, got.Parked())
	assertRetained(t, h)
}

func TestEnsure_UncertainParkWriteIsNotRetried(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		landed bool
	}{
		{name: "park landed and its reply was lost", landed: true},
		{name: "park never landed", landed: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newLaunchHarness()
			s, _ := newTestService(t, h, http.StatusServiceUnavailable, sufficientGPU())
			s.deps.StartupTimeout = 50 * time.Millisecond
			var calls atomic.Int32
			s.parkWrite = func(write func() (uint64, error)) (uint64, error) {
				calls.Add(1)
				if tc.landed {
					_, err := write()
					assert.NoError(t, err)
				}
				return 0, stderrors.New("nats: timeout")
			}

			_, err := s.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")
			require.NoError(t, err)
			s.WaitLaunches()

			assert.EqualValues(t, 1, calls.Load(), "an uncertain park is not retried")
			got, found := storedEndpoint(t, s)
			require.True(t, found, "an uncertain park must not delete the claim")
			if tc.landed {
				assertParkedWithIdentity(t, got, ParkReadinessTimedOut)
			} else {
				assert.Equal(t, StateStarting, got.State)
				assert.False(t, got.Parked())
				assert.Equal(t, "i-1", got.InstanceID, "the identity write still stands")
				assert.EqualValues(t, 2, got.Generation, "nothing after the identity write landed")
			}
			assertRetained(t, h)
		})
	}
}
