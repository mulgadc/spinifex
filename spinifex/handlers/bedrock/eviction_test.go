package handlers_bedrock

import (
	"net/http"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/gpu"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// evictableRecord is a READY endpoint on this node, old enough to be past
// minResidency and quiet — the baseline each case below spoils one field of.
func evictableRecord(modelID string, lastActive time.Time) EndpointRecord {
	return EndpointRecord{
		ModelID:      modelID,
		State:        StateReady,
		NodeID:       testNodeID,
		InstanceID:   "i-" + modelID,
		ReadyAt:      lastActive.Add(-time.Hour),
		LastActiveAt: lastActive,
	}
}

func TestSelectEvictable_PicksLeastRecentlyUsed(t *testing.T) {
	now := time.Now().UTC()
	recs := []EndpointRecord{
		evictableRecord("recent", now.Add(-time.Minute)),
		evictableRecord("stalest", now.Add(-30*time.Minute)),
		evictableRecord("middling", now.Add(-10*time.Minute)),
	}

	victim, found := selectEvictable(recs, testNodeID, time.Minute, now)

	require.True(t, found)
	assert.Equal(t, "stalest", victim.ModelID)
}

// A record the reaper has never sampled falls back to ReadyAt, so a brand-new
// endpoint sorts last rather than first — the opposite would evict the one
// thing that just launched.
func TestSelectEvictable_UnsampledRecordFallsBackToReadyAt(t *testing.T) {
	now := time.Now().UTC()
	fresh := EndpointRecord{ModelID: "fresh", State: StateReady, NodeID: testNodeID, ReadyAt: now.Add(-10 * time.Minute)}
	old := EndpointRecord{ModelID: "old", State: StateReady, NodeID: testNodeID, ReadyAt: now.Add(-3 * time.Hour)}

	victim, found := selectEvictable([]EndpointRecord{fresh, old}, testNodeID, time.Minute, now)

	require.True(t, found)
	assert.Equal(t, "old", victim.ModelID)
}

func TestSelectEvictable_Exclusions(t *testing.T) {
	now := time.Now().UTC()
	stale := now.Add(-time.Hour)

	busy := evictableRecord("busy", stale)
	busy.InFlight = 1

	pinned := evictableRecord("pinned", stale)
	pinned.Pinned = true

	remote := evictableRecord("remote", stale)
	remote.NodeID = "node-elsewhere"

	starting := evictableRecord("starting", stale)
	starting.State = StateStarting

	fresh := evictableRecord("fresh", now)
	fresh.ReadyAt = now.Add(-time.Second)

	tests := []struct {
		name string
		rec  EndpointRecord
	}{
		{"a device is never yanked from a running process", busy},
		{"a pinned endpoint is never evicted", pinned},
		{"another node's endpoint frees no capacity here", remote},
		{"a launch in flight is not ours to stop", starting},
		{"minResidency stops two models thrashing each other", fresh},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, found := selectEvictable([]EndpointRecord{tt.rec}, testNodeID, 5*time.Minute, now)
			assert.False(t, found)
		})
	}
}

// exhaustedGPU reports a device that is present but taken, which is what a
// node running one serving VM on its only GPU looks like.
func exhaustedGPU() *stubSnapshotter {
	return &stubSnapshotter{entries: []gpu.PoolEntry{{Device: gpu.GPUDevice{MemoryMiB: 8192}, Available: false}}}
}

// freeingGPU stays exhausted until the endpoint holding the device is torn
// down, standing in for a real pool whose entry is released by the terminate.
type freeingGPU struct {
	launcher *fakeInstanceLauncher
}

func (f *freeingGPU) Snapshot() []gpu.PoolEntry {
	available := len(f.launcher.terminations()) > 0
	return []gpu.PoolEntry{{Device: gpu.GPUDevice{MemoryMiB: 8192}, Available: available}}
}

// The case that is impossible without eviction: the only GPU is held by a
// model nobody is calling, and every launch that needs it is refused forever.
func TestEnsure_EvictsIdleEndpointToMakeRoom(t *testing.T) {
	h := newLaunchHarness()
	s, _ := newTestService(t, h, 200, &freeingGPU{launcher: h.launcher})
	seedReadyEndpoint(t, s, "other.model-v1:0", time.Now().UTC().Add(-time.Hour))

	out, err := s.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	assert.Equal(t, StateStarting, out.Endpoint.State)
	s.WaitLaunches()

	assert.Contains(t, h.launcher.terminations(), "i-other", "the victim's VM must be torn down to release its device")
	desc, err := s.Describe(t.Context(), &DescribeEndpointInput{ModelID: "other.model-v1:0"}, "")
	require.NoError(t, err)
	assert.Equal(t, StateAbsent, desc.Endpoint.State, "an evicted endpoint's record must be purged")

	desc, err = s.Describe(t.Context(), &DescribeEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	assert.Equal(t, StateReady, desc.Endpoint.State, "the launch the eviction made room for must complete")
}

// Capacity contention breaches no quota, so the refusal that comes back is
// the retryable one, never ServiceQuotaExceededException.
func TestEnsure_NothingEvictableKeepsModelNotReady(t *testing.T) {
	h := newLaunchHarness()
	s, _ := newTestService(t, h, 200, exhaustedGPU())
	// The only other endpoint is pinned, so nothing may be given up.
	rec := seedReadyEndpoint(t, s, "other.model-v1:0", time.Now().UTC().Add(-time.Hour))
	pinEndpoint(t, s, rec.ModelID)

	_, err := s.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorModelNotReadyException, awserrors.ValidErrorCodeFromError(err))
	assert.NotEqual(t, awserrors.ErrorServiceQuotaExceededException, awserrors.ValidErrorCodeFromError(err))
	assert.Empty(t, h.launcher.terminations(), "nothing evictable means nothing is torn down")
	assert.Zero(t, h.launcher.launchCount.Load())
}

// An endpoint still inside minResidency is not a candidate, so a refusal
// stands rather than two models trading the device on every call.
func TestEnsure_MinResidencyProtectsAFreshEndpoint(t *testing.T) {
	h := newLaunchHarness()
	s, _ := newTestService(t, h, 200, exhaustedGPU())
	seedReadyEndpoint(t, s, "other.model-v1:0", time.Now().UTC())

	_, err := s.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorModelNotReadyException, awserrors.ValidErrorCodeFromError(err))
	assert.Empty(t, h.launcher.terminations())
}

// The requester's own endpoint is never the victim: stopping it to launch it
// again is a no-op at best.
func TestEvictForCapacity_NeverEvictsTheRequester(t *testing.T) {
	h := newLaunchHarness()
	s, _ := newTestService(t, h, 200, exhaustedGPU())
	seedReadyEndpoint(t, s, testModelID, time.Now().UTC().Add(-time.Hour))

	assert.False(t, s.evictForCapacity(t.Context(), testModelID, 5120))
	assert.Empty(t, h.launcher.terminations())
}

// TestSelectEvictable_SkipsRecordCreatedPinnedViaEnsure is .7.7's exemption
// guarded against a regression from the new pinned-create path: a record
// Ensure actually wrote with Pinned:true (not just hand-set on a struct) must
// still be invisible to selectEvictable, while an identical unpinned one is
// picked.
func TestSelectEvictable_SkipsRecordCreatedPinnedViaEnsure(t *testing.T) {
	h := newLaunchHarness()
	s, _ := newTestService(t, h, http.StatusOK, sufficientGPU())

	_, err := s.Ensure(t.Context(), &EnsureEndpointInput{
		ModelID: testModelID, AccountID: testAccountID, Pinned: true,
	}, "")
	require.NoError(t, err)
	s.WaitLaunches()

	desc, err := s.Describe(t.Context(), &DescribeEndpointInput{ModelID: testModelID, AccountID: testAccountID}, "")
	require.NoError(t, err)
	pinned := desc.Endpoint
	require.True(t, pinned.Pinned)

	now := time.Now().UTC()
	stale := now.Add(-time.Hour)
	pinned.ReadyAt = stale.Add(-time.Hour)
	pinned.LastActiveAt = stale

	unpinned := pinned
	unpinned.ModelID = "other.model-v1:0"
	unpinned.Pinned = false

	victim, found := selectEvictable([]EndpointRecord{pinned, unpinned}, s.deps.NodeID, 5*time.Minute, now)

	require.True(t, found)
	assert.Equal(t, unpinned.ModelID, victim.ModelID, "the endpoint Ensure created pinned must never be the victim")
}

// seedReadyEndpoint writes a READY record directly, which is the only way to
// stand up a second serving endpoint: the catalog carries one self-host model
// today, so a launched one cannot be joined by a second through Ensure.
func seedReadyEndpoint(t *testing.T, s *Service, modelID string, readyAt time.Time) EndpointRecord {
	t.Helper()
	rec := EndpointRecord{
		AccountID:  utils.GlobalAccountID,
		ModelID:    modelID,
		State:      StateReady,
		NodeID:     s.deps.NodeID,
		InstanceID: "i-other",
		BaseURL:    "http://10.244.1.9:8000",
		ReadyAt:    readyAt,
		Generation: 2,
	}
	_, err := s.store.Create(t.Context(), EndpointKey(utils.GlobalAccountID, modelID), &rec)
	require.NoError(t, err)
	return rec
}

func pinEndpoint(t *testing.T, s *Service, modelID string) {
	t.Helper()
	key := EndpointKey(utils.GlobalAccountID, modelID)
	rec, rev, found, err := s.store.getRevision(t.Context(), key)
	require.NoError(t, err)
	require.True(t, found)
	rec.Pinned = true
	require.NoError(t, s.store.CompareAndSet(t.Context(), key, &rec, rev))
}
