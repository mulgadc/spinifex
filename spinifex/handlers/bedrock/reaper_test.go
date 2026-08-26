package handlers_bedrock

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/config"
	gateway_bedrock "github.com/mulgadc/spinifex/spinifex/gateway/bedrock"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// vllmTransport answers both halves of what a serving VM exposes: the
// readiness probe on /v1/models and the reaper's scrape on /metrics. One
// transport serves both because the Service uses one HTTP client for both.
type vllmTransport struct {
	mu      sync.Mutex
	body    string
	fail    bool
	scrapes int
}

func (v *vllmTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if strings.HasSuffix(req.URL.Path, "/metrics") {
		v.scrapes++
		if v.fail {
			return nil, errors.New("connection refused")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(v.body)),
			Header:     make(http.Header),
		}, nil
	}
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
}

// serve makes the next scrapes report these queue depths and completed count.
func (v *vllmTransport) serve(running, waiting int, successTotal float64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.fail = false
	v.body = fmt.Sprintf("vllm:num_requests_running %d\nvllm:num_requests_waiting %d\nvllm:request_success_total %v\n",
		running, waiting, successTotal)
}

func (v *vllmTransport) breakScrapes() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.fail = true
}

func (v *vllmTransport) scrapeCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.scrapes
}

// reaperFixture is a Service with a reaper bound to it and a scriptable
// serving VM behind both.
type reaperFixture struct {
	svc       *Service
	reaper    *Reaper
	harness   *launchHarness
	transport *vllmTransport
	store     *endpointStore
}

func newReaperFixture(t *testing.T, deps ReaperDeps) *reaperFixture {
	t.Helper()
	h := newLaunchHarness()
	transport := &vllmTransport{}
	transport.serve(0, 0, 0)

	_, nc, _ := testutil.StartTestJetStream(t)
	svc := NewService(nc, ServiceDeps{
		Config:         &config.Config{Region: "us-east-1", AZ: "us-east-1a"},
		Launch:         h.deps(),
		GPU:            sufficientGPU(),
		NodeID:         testNodeID,
		StartupTimeout: 2 * time.Second,
		HTTPClient:     &http.Client{Transport: transport},
		PollInterval:   5 * time.Millisecond,
		Replicas:       1,
	})
	return &reaperFixture{
		svc:       svc,
		reaper:    NewReaper(svc, "node-a", deps),
		harness:   h,
		transport: transport,
		store:     svc.store,
	}
}

// ready launches testModelID and waits for its record to reach READY.
func (f *reaperFixture) ready(t *testing.T) EndpointRecord {
	t.Helper()
	_, err := f.svc.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	f.svc.WaitLaunches()

	desc, err := f.svc.Describe(t.Context(), &DescribeEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	require.Equal(t, StateReady, desc.Endpoint.State)
	return desc.Endpoint
}

// age rewrites the record's timestamps into the past, standing in for an
// endpoint that has been up and quiet for a while.
func (f *reaperFixture) age(t *testing.T, readyAgo, activeAgo time.Duration) EndpointRecord {
	t.Helper()
	key := resolveKey(utils.GlobalAccountID, testModelID)
	rec, rev, found, err := f.store.getRevision(t.Context(), key)
	require.NoError(t, err)
	require.True(t, found)

	now := time.Now().UTC()
	rec.ReadyAt = now.Add(-readyAgo)
	if activeAgo > 0 {
		rec.LastActiveAt = now.Add(-activeAgo)
	}
	require.NoError(t, f.store.CompareAndSet(t.Context(), key, &rec, rev))
	return rec
}

func (f *reaperFixture) current(t *testing.T) (EndpointRecord, bool) {
	t.Helper()
	rec, _, found, err := f.store.getRevision(t.Context(), resolveKey(utils.GlobalAccountID, testModelID))
	require.NoError(t, err)
	return rec, found
}

func TestReaper_ReapsIdleEndpointPastTTL(t *testing.T) {
	f := newReaperFixture(t, ReaperDeps{IdleTTL: time.Minute})
	rec := f.ready(t)
	f.age(t, 10*time.Minute, 10*time.Minute)

	require.NoError(t, f.reaper.sweepOnce(t.Context()))

	_, found := f.current(t)
	assert.False(t, found, "a reaped endpoint's record must be purged, not left DRAINING")
	assert.Contains(t, f.harness.launcher.terminated, rec.InstanceID, "the GPU is only released by tearing the VM down")
}

// The rule that is not implied by the idle clock: an endpoint must not be
// reclaimed in the window between reaching READY and the request that
// launched it arriving.
func TestReaper_NeverReapsWithinIdleTTLOfReadyAt(t *testing.T) {
	f := newReaperFixture(t, ReaperDeps{IdleTTL: time.Hour})
	f.ready(t)
	// Idle for far longer than the TTL, but only just became READY.
	f.age(t, time.Second, 24*time.Hour)

	require.NoError(t, f.reaper.sweepOnce(t.Context()))

	_, found := f.current(t)
	assert.True(t, found, "a freshly READY endpoint must survive its first sweep")
	assert.Empty(t, f.harness.launcher.terminated)
}

func TestReaper_NeverReapsPinnedEndpoint(t *testing.T) {
	f := newReaperFixture(t, ReaperDeps{IdleTTL: time.Minute})
	f.ready(t)
	f.age(t, time.Hour, time.Hour)

	key := resolveKey(utils.GlobalAccountID, testModelID)
	rec, rev, _, err := f.store.getRevision(t.Context(), key)
	require.NoError(t, err)
	rec.Pinned = true
	require.NoError(t, f.store.CompareAndSet(t.Context(), key, &rec, rev))

	require.NoError(t, f.reaper.sweepOnce(t.Context()))

	got, found := f.current(t)
	require.True(t, found)
	assert.True(t, got.Pinned)
	assert.Empty(t, f.harness.launcher.terminated)
}

// TestReaper_ShouldReap_SkipsRecordCreatedPinnedViaEnsure is .7.7's exemption
// guarded against a regression from the new pinned-create path: a record
// Ensure actually wrote with Pinned:true (not just hand-set on a struct) must
// still fail shouldReap once idle past the TTL, while an identical unpinned
// one is reapable.
func TestReaper_ShouldReap_SkipsRecordCreatedPinnedViaEnsure(t *testing.T) {
	f := newReaperFixture(t, ReaperDeps{IdleTTL: time.Minute})

	_, err := f.svc.Ensure(t.Context(), &EnsureEndpointInput{
		ModelID: testModelID, AccountID: testAccountID, Pinned: true,
	}, "")
	require.NoError(t, err)
	f.svc.WaitLaunches()

	desc, err := f.svc.Describe(t.Context(), &DescribeEndpointInput{ModelID: testModelID, AccountID: testAccountID}, "")
	require.NoError(t, err)
	pinned := desc.Endpoint
	require.True(t, pinned.Pinned)

	now := time.Now().UTC()
	pinned.ReadyAt = now.Add(-time.Hour)
	pinned.LastActiveAt = now.Add(-time.Hour)

	unpinned := pinned
	unpinned.Pinned = false

	assert.False(t, f.reaper.shouldReap(pinned, now), "the endpoint Ensure created pinned must never be reaped")
	assert.True(t, f.reaper.shouldReap(unpinned, now), "an identical unpinned record must still be reapable")
}

// A busy endpoint's LastActiveAt is refreshed rather than left to age out,
// which is both what stops the reap and what orders eviction later.
func TestReaper_BusyEndpointStampsLastActive(t *testing.T) {
	f := newReaperFixture(t, ReaperDeps{IdleTTL: time.Minute})
	f.ready(t)
	f.age(t, time.Hour, time.Hour)
	f.transport.serve(2, 1, 5)

	require.NoError(t, f.reaper.sweepOnce(t.Context()))

	got, found := f.current(t)
	require.True(t, found, "an endpoint serving traffic must never be reaped")
	assert.WithinDuration(t, time.Now(), got.LastActiveAt, time.Minute)
	assert.Equal(t, 3, got.InFlight)
	assert.InDelta(t, 5.0, got.SuccessTotal, 0)
}

// Both queues empty but the counter advanced means a request landed and
// completed between sweeps, which is activity, not idleness.
func TestReaper_AdvancedCounterCountsAsActivity(t *testing.T) {
	f := newReaperFixture(t, ReaperDeps{IdleTTL: time.Minute})
	f.ready(t)
	f.age(t, time.Hour, time.Hour)
	f.transport.serve(0, 0, 12)

	require.NoError(t, f.reaper.sweepOnce(t.Context()))

	got, found := f.current(t)
	require.True(t, found)
	assert.WithinDuration(t, time.Now(), got.LastActiveAt, time.Minute)

	// Once that activity has aged out, the same unchanged counter is what makes
	// the next sweep read the endpoint as idle.
	f.age(t, time.Hour, time.Hour)
	require.NoError(t, f.reaper.sweepOnce(t.Context()))
	_, found = f.current(t)
	assert.False(t, found)
}

func TestReaper_ScrapeFailuresEscalateToRelaunch(t *testing.T) {
	f := newReaperFixture(t, ReaperDeps{IdleTTL: time.Hour})
	rec := f.ready(t)
	f.transport.breakScrapes()

	for range maxScrapeFailures - 1 {
		require.NoError(t, f.reaper.sweepOnce(t.Context()))
		got, found := f.current(t)
		require.True(t, found, "an unreachable endpoint is unknown, never idle")
		assert.Empty(t, f.harness.launcher.terminated)
		assert.Positive(t, got.ScrapeFailures)
	}

	// The escalating sweep tears the wedged VM down and asks for it back.
	f.transport.serve(0, 0, 0)
	f.transport.breakScrapes()
	require.NoError(t, f.reaper.sweepOnce(t.Context()))
	f.svc.WaitLaunches()

	assert.Contains(t, f.harness.launcher.terminated, rec.InstanceID)
	assert.EqualValues(t, 2, f.harness.launcher.launchCount.Load(), "the endpoint must be relaunched, not just removed")
}

// A pinned endpoint is operator/commitment-managed: a persistent scrape
// failure must never make the reaper tear it down, exactly as it never
// idle-reclaims one.
func TestReaper_PinnedEndpointNotReapedOnScrapeFailure(t *testing.T) {
	f := newReaperFixture(t, ReaperDeps{IdleTTL: time.Hour})
	f.ready(t)

	key := resolveKey(utils.GlobalAccountID, testModelID)
	rec, rev, found, err := f.store.getRevision(t.Context(), key)
	require.NoError(t, err)
	require.True(t, found)
	rec.Pinned = true
	require.NoError(t, f.store.CompareAndSet(t.Context(), key, &rec, rev))

	f.transport.breakScrapes()
	for range maxScrapeFailures + 2 {
		require.NoError(t, f.reaper.sweepOnce(t.Context()))
	}

	got, found := f.current(t)
	require.True(t, found, "a pinned endpoint is never reaped on scrape failure")
	assert.Empty(t, f.harness.launcher.terminated)
	assert.Positive(t, got.ScrapeFailures, "the failure is still counted and logged")
}

// A service-only bundle (no generative vLLM member) exposes no vLLM load
// metrics, so it is liveness-probed on /health and kept warm rather than
// idle-scraped and reaped — even with an aggressive idle TTL and aged
// timestamps that would reap a vLLM endpoint instantly.
func TestReaper_ServiceOnlyBundleLivenessProbedNotReaped(t *testing.T) {
	f := newReaperFixture(t, ReaperDeps{IdleTTL: time.Nanosecond})

	const embedModelID = "embed-only-bundle"
	key := resolveKey(utils.GlobalAccountID, embedModelID)
	rec := EndpointRecord{
		AccountID:    utils.GlobalAccountID,
		ModelID:      embedModelID,
		State:        StateReady,
		BaseURL:      "http://10.0.0.1:8001",
		PrivateIP:    "10.0.0.1",
		Members:      map[string]MemberEndpoint{embedModelID: {Port: 8001, Family: gateway_bedrock.FamilyTEI}},
		ReadyAt:      time.Now().Add(-time.Hour),
		LastActiveAt: time.Now().Add(-time.Hour),
		Generation:   1,
	}
	_, err := f.store.Create(t.Context(), key, &rec)
	require.NoError(t, err)

	require.NoError(t, f.reaper.sweepOnce(t.Context()))

	rec, rev, found, err := f.store.getRevision(t.Context(), key)
	require.NoError(t, err)
	require.True(t, found, "a warm service bundle is never idle-reaped")
	assert.Empty(t, f.harness.launcher.terminated)
	assert.Zero(t, f.transport.scrapeCount(), "a service-only bundle is never scraped for vLLM metrics")
	assert.Zero(t, rec.ScrapeFailures, "a healthy /health probe leaves no failures")
	_ = rev
}

func TestReaper_SuccessfulScrapeResetsFailureCount(t *testing.T) {
	f := newReaperFixture(t, ReaperDeps{IdleTTL: time.Hour})
	f.ready(t)

	f.transport.breakScrapes()
	require.NoError(t, f.reaper.sweepOnce(t.Context()))
	got, _ := f.current(t)
	require.Equal(t, 1, got.ScrapeFailures)

	f.transport.serve(1, 0, 0)
	require.NoError(t, f.reaper.sweepOnce(t.Context()))
	got, _ = f.current(t)
	assert.Zero(t, got.ScrapeFailures)
}

// A steady-state idle endpoint is the common case and must cost a scrape per
// tick and nothing else; a KV write per endpoint per tick forever is the
// failure mode being guarded against.
func TestReaper_UnchangedObservationWritesNothing(t *testing.T) {
	f := newReaperFixture(t, ReaperDeps{IdleTTL: time.Hour})
	f.ready(t)

	before, _ := f.current(t)
	require.NoError(t, f.reaper.sweepOnce(t.Context()))
	require.NoError(t, f.reaper.sweepOnce(t.Context()))
	after, found := f.current(t)

	require.True(t, found)
	assert.Equal(t, before.Generation, after.Generation, "an unchanged observation must not bump the record")
	assert.Equal(t, 2, f.transport.scrapeCount(), "the endpoint is still scraped every tick")
}

// STARTING belongs to the launch goroutine and DRAINING to Delete; the reaper
// scrapes neither, so a launch in progress cannot be torn down under it.
func TestReaper_SkipsNonReadyRecords(t *testing.T) {
	f := newReaperFixture(t, ReaperDeps{IdleTTL: time.Nanosecond})
	key := resolveKey(utils.GlobalAccountID, testModelID)
	rec := EndpointRecord{
		AccountID: utils.GlobalAccountID, ModelID: testModelID, State: StateStarting, Generation: 1,
	}
	_, err := f.store.Create(t.Context(), key, &rec)
	require.NoError(t, err)

	require.NoError(t, f.reaper.sweepOnce(t.Context()))

	_, found := f.current(t)
	assert.True(t, found)
	assert.Zero(t, f.transport.scrapeCount())
}

func TestReaper_LeaseAdmitsOneLeader(t *testing.T) {
	f := newReaperFixture(t, ReaperDeps{})
	other := NewReaper(f.svc, "node-b", ReaperDeps{})

	require.True(t, f.reaper.lease.TryAcquire(t.Context()))
	require.False(t, other.lease.TryAcquire(t.Context()),
		"two nodes must never sweep the same endpoints at once")

	assert.True(t, f.reaper.IsLeader())
	assert.False(t, other.IsLeader())

	// A refresh by the holder keeps it, and does not hand it away.
	require.True(t, f.reaper.lease.TryAcquire(t.Context()))
	assert.True(t, f.reaper.IsLeader())
}

// Releasing on shutdown is what makes a rolling restart cost seconds rather
// than a full lease TTL of no reclaim.
func TestReaper_ReleaseFreesTheLeaseImmediately(t *testing.T) {
	f := newReaperFixture(t, ReaperDeps{})
	other := NewReaper(f.svc, "node-b", ReaperDeps{})

	f.reaper.lease.TryAcquire(t.Context())
	require.True(t, f.reaper.IsLeader())

	f.reaper.lease.Release(t.Context())
	other.lease.TryAcquire(t.Context())
	assert.True(t, other.IsLeader())
}

// A node that lost the election does no work at all, so a leaderless gap
// delays a reclaim rather than duplicating one.
func TestReaper_NonLeaderSweepsNothing(t *testing.T) {
	f := newReaperFixture(t, ReaperDeps{Interval: 5 * time.Millisecond, IdleTTL: time.Nanosecond})
	f.ready(t)
	f.age(t, time.Hour, time.Hour)

	holder := NewReaper(f.svc, "node-b", ReaperDeps{})
	holder.lease.TryAcquire(t.Context())
	require.True(t, holder.IsLeader())

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Millisecond)
	defer cancel()
	f.reaper.Run(ctx)

	_, found := f.current(t)
	assert.True(t, found, "a non-leader must not reap")
	assert.Zero(t, f.transport.scrapeCount())
}

// LastActive is the one definition both the idle clock and the LRU eviction
// order read, so they cannot drift apart.
func TestEndpointRecord_LastActive(t *testing.T) {
	ready := time.Now().UTC().Add(-time.Hour)
	active := ready.Add(30 * time.Minute)

	assert.Equal(t, active, EndpointRecord{ReadyAt: ready, LastActiveAt: active}.LastActive())
	assert.Equal(t, ready, EndpointRecord{ReadyAt: ready}.LastActive(),
		"an endpoint quiet since launch is idle since launch, not since the zero time")
}

// Shutdown must delete the lease key before Run returns. The daemon waits on Run
// via its shutdown group, so a key left behind parks the next election for the
// full TTL rather than freeing it immediately.
func TestReaper_RunReleasesLeaseOnShutdown(t *testing.T) {
	f := newReaperFixture(t, ReaperDeps{Interval: time.Hour})
	require.NoError(t, f.reaper.leaseErr)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); f.reaper.Run(ctx) }()
	require.Eventually(t, f.reaper.IsLeader, 2*time.Second, 20*time.Millisecond)

	cancel()
	<-done

	other := NewReaper(f.svc, "node-b", ReaperDeps{})
	assert.True(t, other.lease.TryAcquire(t.Context()),
		"Run returned with the lease key still present")
}
