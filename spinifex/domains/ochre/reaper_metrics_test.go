package ochre

//test:in-package: drives the reaper sweep through the scriptable vLLM
// transport and rewrites the stored record's members directly.

import (
	"strings"
	"testing"
	"time"

	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A 200 that is not the complete supported sample for the primary member is
// not evidence of an idle server: the endpoint stays held, idle past its TTL.
func TestReaper_IncompleteOrInvalidMetricsKeepTheHold(t *testing.T) {
	t.Parallel()
	lines := vllmMetricsLines(testModelID, 0, 0, 0)
	bodies := map[string]string{
		"running missing":             editLines(lines, map[string]string{gaugeLine(metricRunning): ""}),
		"waiting missing":             editLines(lines, map[string]string{gaugeLine(metricWaiting): ""}),
		"success missing":             editLines(lines, map[string]string{metricSuccessTotal + "{": ""}),
		"finish reason missing":       editLines(lines, map[string]string{successLine("repetition"): ""}),
		"malformed":                   editLines(lines, map[string]string{gaugeLine(metricRunning): gaugeLine(metricRunning) + " not-a-number"}),
		"non-finite":                  editLines(lines, map[string]string{gaugeLine(metricWaiting): gaugeLine(metricWaiting) + " +Inf"}),
		"fractional":                  editLines(lines, map[string]string{gaugeLine(metricRunning): gaugeLine(metricRunning) + " 0.5"}),
		"out of range":                editLines(lines, map[string]string{successLine("stop"): successLine("stop") + " 1e+17"}),
		"duplicate":                   vllmMetricsBody(testModelID, 0, 0, 0) + successLine("stop") + " 0.0\n",
		"families agree, wrong model": vllmMetricsBody("other.model-v1:0", 0, 0, 0),
		"engine omitted everywhere": editLines(lines, map[string]string{
			gaugeLine(metricRunning): metricRunning + `{model_name="` + testModelID + `"} 0.0`,
			gaugeLine(metricWaiting): metricWaiting + `{model_name="` + testModelID + `"} 0.0`,
			metricSuccessTotal + "{": "",
		}),
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, rec := idleFixture(t)
			f.transport.serveBody(body)

			require.NoError(t, f.reaper.sweepOnce(t.Context(), nil))

			cur := assertUntouched(t, f, rec.InstanceID)
			assert.True(t, cur.Held(), "an incomplete sample must leave the hold")
			assert.Equal(t, 1, f.transport.scrapeCount())
		})
	}
}

// The success count can move between finish reasons while its sum stays put;
// a scrape missing one reason still keeps the hold, not reclaims as idle.
func TestReaper_MissingFinishReasonWithUnchangedTotalKeepsTheHold(t *testing.T) {
	t.Parallel()
	f, rec := idleFixture(t)
	lines := vllmMetricsLines(testModelID, 0, 0, 0)
	f.transport.serveBody(editLines(lines, map[string]string{
		successLine("stop"): successLine("stop") + " 3.0", successLine("length"): successLine("length") + " 1.0",
	}))
	require.NoError(t, f.reaper.sweepOnce(t.Context(), nil))
	first := assertUntouched(t, f, rec.InstanceID)
	require.False(t, first.Held())
	require.InDelta(t, 4.0, first.SuccessTotal, 0)

	f.age(t, 10*time.Minute, 10*time.Minute)
	f.transport.serveBody(editLines(lines, map[string]string{
		successLine("stop"): successLine("stop") + " 4.0", successLine("length"): "",
	}))
	require.NoError(t, f.reaper.sweepOnce(t.Context(), nil))

	cur := assertUntouched(t, f, rec.InstanceID)
	assert.True(t, cur.Held())
	assert.InDelta(t, 4.0, cur.SuccessTotal, 0)
}

// A record whose primary vLLM member cannot be identified is never scraped
// against a guessed model, and stays held until it is resolved.
func TestReaper_UnknownPrimaryIdentityKeepsTheHold(t *testing.T) {
	t.Parallel()
	cases := map[string]map[string]MemberEndpoint{
		"no members":       nil,
		"two vLLM members": {testModelID: {Port: 8000, Family: FamilyMeta}, "meta.other-v1:0": {Port: 8001, Family: FamilyMeta}},
	}
	for name, members := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, rec := idleFixture(t)
			key := resolveKey(awsidentifiers.GlobalAccountID, testModelID)
			stored, rev := readRecord(t, f.svc, key)
			stored.Members = members
			_, err := f.store.CompareAndSet(t.Context(), key, &stored, rev)
			require.NoError(t, err)

			require.NoError(t, f.reaper.sweepOnce(t.Context(), nil))

			cur := assertUntouched(t, f, rec.InstanceID)
			assert.True(t, cur.Held())
			assert.Zero(t, f.transport.scrapeCount())
		})
	}
}

// storeMembers rewrites the stored record through edit, as a stale or partial
// write left by an earlier daemon would.
func storeMembers(t *testing.T, f *reaperFixture, edit func(*EndpointRecord)) {
	t.Helper()
	key := resolveKey(awsidentifiers.GlobalAccountID, testModelID)
	stored, rev := readRecord(t, f.svc, key)
	edit(&stored)
	_, err := f.store.CompareAndSet(t.Context(), key, &stored, rev)
	require.NoError(t, err)
}

// assertNotEvictable shows a capacity eviction for another model leaves rec's
// VM running; the fixture's residency is short enough that it otherwise would.
func assertNotEvictable(t *testing.T, f *reaperFixture, rec EndpointRecord) {
	t.Helper()
	assert.False(t, f.svc.evictForCapacity(t.Context(), catalogVictimModel, 5120))
	assertUntouched(t, f, rec.InstanceID)
}

// A record whose address or members do not match its catalog group is not
// probed at all: a stale address may reach another server that answers a
// complete idle sample, and a missing vLLM member must not pass as service-only.
func TestReaper_UnverifiableBundleIsHeldAndNotEvicted(t *testing.T) {
	t.Parallel()
	edits := map[string]func(*EndpointRecord){
		"stale base URL on another host": func(r *EndpointRecord) { r.BaseURL = "http://10.9.9.9:8000" },
		"base URL on a service member":   func(r *EndpointRecord) { r.BaseURL = "http://" + r.PrivateIP + ":8001" },
		"expected vLLM member missing":   func(r *EndpointRecord) { delete(r.Members, testModelID) },
		"vLLM member recorded as a service": func(r *EndpointRecord) {
			m := r.Members[testModelID]
			m.Family = FamilyTEI
			r.Members[testModelID] = m
		},
	}
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, rec := idleFixture(t)
			f.svc.deps.MinResidency = time.Minute
			f.transport.serve(0, 0, 0)
			storeMembers(t, f, edit)
			before := f.transport.requestCount()

			require.NoError(t, f.reaper.sweepOnce(t.Context(), nil))

			cur := assertUntouched(t, f, rec.InstanceID)
			assert.True(t, cur.Held(), "an unverifiable record must stay held")
			assert.Equal(t, before, f.transport.requestCount(), "an unverifiable address must not be probed")
			assertNotEvictable(t, f, rec)
		})
	}
}

// The same fixture, verified and idle, is reclaimed by the sweep: the refusals
// above are the identity check, not a fixture that can never be reclaimed.
func TestReaper_VerifiedIdleBundleIsReclaimed(t *testing.T) {
	t.Parallel()
	f, rec := idleFixture(t)
	f.transport.serve(0, 0, 0)

	require.NoError(t, f.reaper.sweepOnce(t.Context(), nil))

	f.svc.WaitLaunches()
	_, found := f.current(t)
	assert.False(t, found)
	assert.Contains(t, f.harness.launcher.terminations(), rec.InstanceID)
}

// A cleared, verified bundle is a capacity-eviction candidate, so the held
// cases' refusal to evict is the hold, not the fixture's residency.
func TestReaper_VerifiedClearedBundleIsEvictable(t *testing.T) {
	t.Parallel()
	f := newReaperFixture(t, ReaperDeps{IdleTTL: 24 * time.Hour})
	rec := f.ready(t)
	f.age(t, 10*time.Minute, 10*time.Minute)
	f.svc.deps.MinResidency = time.Minute
	f.transport.serve(0, 0, 0)
	require.NoError(t, f.reaper.sweepOnce(t.Context(), nil))
	cur, found := f.current(t)
	require.True(t, found)
	require.False(t, cur.Held())

	assert.True(t, f.svc.evictForCapacity(t.Context(), catalogVictimModel, 5120))
	f.svc.WaitLaunches()
	assert.Contains(t, f.harness.launcher.terminations(), rec.InstanceID)
}

// A complete sample padded to the read limit clears the hold; one byte more,
// with no declared length, keeps it, though the bytes before the limit are a
// valid scrape.
func TestReaper_OversizedMetricsKeepTheHold(t *testing.T) {
	t.Parallel()
	padded := func(size int) string {
		body := vllmMetricsBody(testModelID, 0, 0, 0) + "# "
		return body + strings.Repeat("x", size-len(body)-1) + "\n"
	}
	for name, tc := range map[string]struct {
		size     int
		wantHeld bool
	}{
		"at the limit":     {metricsBodyLimit, false},
		"one byte past it": {metricsBodyLimit + 1, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newReaperFixture(t, ReaperDeps{IdleTTL: 24 * time.Hour})
			rec := f.ready(t)
			body := padded(tc.size)
			require.Len(t, body, tc.size)
			f.transport.serveBody(body)

			require.NoError(t, f.reaper.sweepOnce(t.Context(), nil))

			cur := assertUntouched(t, f, rec.InstanceID)
			assert.Equal(t, tc.wantHeld, cur.Held())
			assert.Equal(t, 1, f.transport.scrapeCount())
		})
	}
}

// vLLM's success counter is a float: at 2^53 a further request can leave it
// unchanged, so that value keeps the hold while 2^53-1 clears it.
func TestReaper_SuccessCountExactBoundary(t *testing.T) {
	t.Parallel()
	for value, wantHeld := range map[string]bool{"9007199254740991": false, "9007199254740992": true} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			f := newReaperFixture(t, ReaperDeps{IdleTTL: 24 * time.Hour})
			rec := f.ready(t)
			f.transport.serveBody(editLines(vllmMetricsLines(testModelID, 0, 0, 0),
				map[string]string{successLine("stop"): successLine("stop") + " " + value}))

			require.NoError(t, f.reaper.sweepOnce(t.Context(), nil))

			cur := assertUntouched(t, f, rec.InstanceID)
			assert.Equal(t, wantHeld, cur.Held())
			if !wantHeld {
				assert.InDelta(t, float64(1<<53-1), cur.SuccessTotal, 0)
			}
		})
	}
}
