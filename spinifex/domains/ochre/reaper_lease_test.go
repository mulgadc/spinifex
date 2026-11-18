package ochre

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvsync"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gatedTransport holds the first /metrics scrape after arm until release
// closes, so a test can change the world while a sweep is mid-scrape.
type gatedTransport struct {
	inner   http.RoundTripper
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (f *reaperFixture) gateNextScrape() *gatedTransport {
	g := &gatedTransport{inner: f.transport, entered: make(chan struct{}), release: make(chan struct{})}
	g.armed.Store(true)
	f.svc.deps.HTTPClient = &http.Client{Transport: g}
	return g
}

func (g *gatedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/metrics") && g.armed.CompareAndSwap(true, false) {
		close(g.entered)
		<-g.release
	}
	return g.inner.RoundTrip(req)
}

// shortLease gives the fixture's reaper a 300 ms lease, so a renewal notices a
// taken key within a test, and returns the sync bucket the lease lives in.
func (f *reaperFixture) shortLease(t *testing.T) jetstream.KeyValue {
	t.Helper()
	js, err := jetstream.New(f.svc.nc)
	require.NoError(t, err)
	store := kvstore.OpenSyncBucket(js)
	f.reaper = NewReaper(f.svc, kvsync.NewLease(store, KeyReaper, "node-a", 300*time.Millisecond), f.reaper.deps)
	syncKV, err := store.KV(t.Context())
	require.NoError(t, err)
	return syncKV
}

// stealLease hands the reaper key to another holder and waits for the
// fixture's reaper to notice.
func (f *reaperFixture) stealLease(t *testing.T, syncKV jetstream.KeyValue) {
	t.Helper()
	_, err := syncKV.Put(t.Context(), KeyReaper, []byte(`{"holder":"node-b","generation":0}`))
	require.NoError(t, err)
	require.Eventually(t, func() bool { return !f.reaper.IsLeader() }, 5*time.Second, time.Millisecond)
}

// sweepAcrossALeaseLoss runs one leader's tick whose scrape is held until
// another holder has taken the lease, then lets the scrape answer and waits
// for the tick to finish.
func (f *reaperFixture) sweepAcrossALeaseLoss(t *testing.T) {
	t.Helper()
	syncKV := f.shortLease(t)
	lead(t, f.reaper)
	gate := f.gateNextScrape()

	swept := make(chan struct{})
	go func() { defer close(swept); f.reaper.tick(t.Context()) }()
	<-gate.entered
	f.stealLease(t, syncKV)
	close(gate.release)
	<-swept
}

// ADR-0003:S7 invariant for the idle-reclaim path: a sweep that read an idle
// endpoint as leader and lost the lease before acting leaves it to the new
// leader, so two nodes never tear down the same serving VM.
func TestReaper_ADR0003S7_IdleReclaimStopsWhenTheLeaseIsLostMidSweep(t *testing.T) {
	t.Parallel()
	f := newReaperFixture(t, ReaperDeps{IdleTTL: time.Minute})
	rec := f.ready(t)
	f.age(t, 10*time.Minute, 10*time.Minute)

	f.sweepAcrossALeaseLoss(t)

	_, found := f.current(t)
	assert.True(t, found, "a sweep that lost the lease must not reclaim")
	assert.NotContains(t, f.harness.launcher.terminations(), rec.InstanceID)
}

// ADR-0003:S7 invariant for the unreachable path: the escalating scrape
// failure of a sweep that lost the lease neither terminates nor relaunches.
func TestReaper_ADR0003S7_UnreachableEscalationStopsWhenTheLeaseIsLostMidSweep(t *testing.T) {
	t.Parallel()
	f := newReaperFixture(t, ReaperDeps{IdleTTL: time.Hour})
	rec := f.ready(t)
	f.transport.breakScrapes()
	for range maxScrapeFailures - 1 {
		require.NoError(t, f.reaper.sweepOnce(t.Context(), nil))
	}

	f.sweepAcrossALeaseLoss(t)
	f.svc.WaitLaunches()

	_, found := f.current(t)
	assert.True(t, found, "a sweep that lost the lease must not terminate an unreachable endpoint")
	assert.NotContains(t, f.harness.launcher.terminations(), rec.InstanceID)
	assert.EqualValues(t, 1, f.harness.launcher.launchCount.Load(), "a sweep that lost the lease must not relaunch")
}

// replace deletes the endpoint and launches it again, returning the old and
// new records, which a launch numbers with the same generation.
func (f *reaperFixture) replace(t *testing.T) (old, replacement EndpointRecord) {
	t.Helper()
	old, _ = f.current(t)
	_, err := f.svc.Delete(t.Context(), &DeleteEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	f.ready(t)
	replacement, _ = f.current(t)
	require.NotEqual(t, old.InstanceID, replacement.InstanceID)
	require.Equal(t, old.Generation, replacement.Generation, "the generation alone must not tell the two apart")
	return old, replacement
}

// ADR-0003:S7 invariant: a reclaim that reaches the record after the endpoint
// was relaunched must not tear the replacement down.
func TestService_ADR0003S7_ReclaimRefusesAReplacedEndpoint(t *testing.T) {
	t.Parallel()
	f := newReaperFixture(t, ReaperDeps{})
	f.ready(t)
	old, replacement := f.replace(t)

	err := f.svc.reclaim(t.Context(), old, func() bool { return true })
	require.ErrorIs(t, err, errReclaimSuperseded)

	cur, found := f.current(t)
	require.True(t, found)
	assert.Equal(t, StateReady, cur.State)
	assert.Equal(t, replacement.InstanceID, cur.InstanceID)
	assert.NotContains(t, f.harness.launcher.terminations(), replacement.InstanceID)
}

// ADR-0003:S7 invariant: leadership is judged once the key is locked, so a
// reclaim that waited out another operation and lost the lease meanwhile
// terminates nothing.
func TestService_ADR0003S7_ReclaimRefusesOnceTheLeaseIsLost(t *testing.T) {
	t.Parallel()
	f := newReaperFixture(t, ReaperDeps{})
	rec := f.ready(t)
	key := resolveKey(awsidentifiers.GlobalAccountID, testModelID)

	var askedUnderLock bool
	stillLeads := func() bool {
		f.svc.ensureMu.mu.Lock()
		m := f.svc.ensureMu.locks[key]
		f.svc.ensureMu.mu.Unlock()
		if m.TryLock() {
			m.Unlock()
		} else {
			askedUnderLock = true
		}
		return false
	}
	require.ErrorIs(t, f.svc.reclaim(t.Context(), rec, stillLeads), errReclaimSuperseded)

	assert.True(t, askedUnderLock, "leadership must be judged under the key's mutex")
	cur, found := f.current(t)
	require.True(t, found)
	assert.Equal(t, StateReady, cur.State)
	assert.Empty(t, f.harness.launcher.terminations())
}

// ADR-0003:S7 invariant: a reclaim admitted while leading that commits after a
// lease loss yields to anything the new leader wrote, since that moves the
// revision the draining CAS is fenced on.
func TestService_ADR0003S7_ReclaimAdmittedBeforeALeaseLossYieldsToTheNewLeader(t *testing.T) {
	t.Parallel()
	f := newReaperFixture(t, ReaperDeps{})
	rec := f.ready(t)
	key := resolveKey(awsidentifiers.GlobalAccountID, testModelID)

	observedAt := time.Now().UTC().Truncate(time.Second)
	stillLeads := func() bool {
		cur, rev, found, err := f.store.getRevision(t.Context(), key)
		require.NoError(t, err)
		require.True(t, found)
		cur.LastActiveAt = observedAt
		_, err = f.store.CompareAndSet(t.Context(), key, &cur, rev)
		require.NoError(t, err)
		return true
	}
	require.ErrorIs(t, f.svc.reclaim(t.Context(), rec, stillLeads), errReclaimSuperseded)

	cur, found := f.current(t)
	require.True(t, found)
	assert.Equal(t, StateReady, cur.State)
	assert.True(t, cur.LastActiveAt.Equal(observedAt), "the new leader's observation must survive")
	assert.Empty(t, f.harness.launcher.terminations())
}

// An observation read before the endpoint was relaunched must not land on the
// replacement, which would restore the old VM's identity over the new one.
func TestReaper_StaleObservationDoesNotOverwriteAReplacement(t *testing.T) {
	t.Parallel()
	f := newReaperFixture(t, ReaperDeps{IdleTTL: time.Hour})
	f.ready(t)
	f.transport.serve(1, 0, 0)
	gate := f.gateNextScrape()

	swept := make(chan struct{})
	go func() { defer close(swept); assert.NoError(t, f.reaper.sweepOnce(t.Context(), nil)) }()
	<-gate.entered
	_, replacement := f.replace(t)
	close(gate.release)
	<-swept

	cur, found := f.current(t)
	require.True(t, found)
	assert.Equal(t, replacement.InstanceID, cur.InstanceID)
	assert.Equal(t, replacement.Generation, cur.Generation)
	assert.Zero(t, cur.InFlight, "the stale sweep's observation must not be written")
}
