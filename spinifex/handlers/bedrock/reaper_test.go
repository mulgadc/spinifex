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
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go/jetstream"
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
	kv        jetstream.KeyValue
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
	kv, err := svc.bucket(t.Context())
	require.NoError(t, err)

	return &reaperFixture{
		svc:       svc,
		reaper:    NewReaper(svc, "node-a", deps),
		harness:   h,
		transport: transport,
		kv:        kv,
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
	key := EndpointKey(utils.GlobalAccountID, testModelID)
	rec, rev, found, err := getFullJSON(t.Context(), f.kv, key)
	require.NoError(t, err)
	require.True(t, found)

	now := time.Now().UTC()
	rec.ReadyAt = now.Add(-readyAgo)
	if activeAgo > 0 {
		rec.LastActiveAt = now.Add(-activeAgo)
	}
	require.NoError(t, updateJSON(t.Context(), f.kv, key, rev, rec))
	return rec
}

func (f *reaperFixture) current(t *testing.T) (EndpointRecord, bool) {
	t.Helper()
	rec, _, found, err := getFullJSON(t.Context(), f.kv, EndpointKey(utils.GlobalAccountID, testModelID))
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

	key := EndpointKey(utils.GlobalAccountID, testModelID)
	rec, rev, _, err := getFullJSON(t.Context(), f.kv, key)
	require.NoError(t, err)
	rec.Pinned = true
	require.NoError(t, updateJSON(t.Context(), f.kv, key, rev, rec))

	require.NoError(t, f.reaper.sweepOnce(t.Context()))

	got, found := f.current(t)
	require.True(t, found)
	assert.True(t, got.Pinned)
	assert.Empty(t, f.harness.launcher.terminated)
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
	key := EndpointKey(utils.GlobalAccountID, testModelID)
	_, err := createJSONRevision(t.Context(), f.kv, key, EndpointRecord{
		AccountID: utils.GlobalAccountID, ModelID: testModelID, State: StateStarting, Generation: 1,
	})
	require.NoError(t, err)

	require.NoError(t, f.reaper.sweepOnce(t.Context()))

	_, found := f.current(t)
	assert.True(t, found)
	assert.Zero(t, f.transport.scrapeCount())
}

func TestReaper_LeaseAdmitsOneLeader(t *testing.T) {
	f := newReaperFixture(t, ReaperDeps{})
	other := NewReaper(f.svc, "node-b", ReaperDeps{})

	f.reaper.evaluateLeadership(t.Context())
	other.evaluateLeadership(t.Context())

	assert.True(t, f.reaper.IsLeader())
	assert.False(t, other.IsLeader(), "two nodes must never sweep the same endpoints at once")

	// A refresh by the holder keeps it, and does not hand it away.
	f.reaper.evaluateLeadership(t.Context())
	assert.True(t, f.reaper.IsLeader())
}

// Releasing on shutdown is what makes a rolling restart cost seconds rather
// than a full lease TTL of no reclaim.
func TestReaper_RelinquishFreesTheLeaseImmediately(t *testing.T) {
	f := newReaperFixture(t, ReaperDeps{})
	other := NewReaper(f.svc, "node-b", ReaperDeps{})

	f.reaper.evaluateLeadership(t.Context())
	require.True(t, f.reaper.IsLeader())

	f.reaper.relinquish()
	other.evaluateLeadership(t.Context())
	assert.True(t, other.IsLeader())
}

// A node that lost the election does no work at all, so a leaderless gap
// delays a reclaim rather than duplicating one.
func TestReaper_NonLeaderSweepsNothing(t *testing.T) {
	f := newReaperFixture(t, ReaperDeps{Interval: 5 * time.Millisecond, LeaseRefresh: time.Hour, IdleTTL: time.Nanosecond})
	f.ready(t)
	f.age(t, time.Hour, time.Hour)

	holder := NewReaper(f.svc, "node-b", ReaperDeps{})
	holder.evaluateLeadership(t.Context())
	require.True(t, holder.IsLeader())

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Millisecond)
	defer cancel()
	f.reaper.Run(ctx)

	_, found := f.current(t)
	assert.True(t, found, "a non-leader must not reap")
	assert.Zero(t, f.transport.scrapeCount())
}
