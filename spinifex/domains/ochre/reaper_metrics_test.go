package ochre

//test:in-package: drives the reaper sweep through the scriptable vLLM
// transport and rewrites the stored record's members directly.

import (
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
		"running missing":           editLines(lines, map[string]string{gaugeLine(metricRunning): ""}),
		"waiting missing":           editLines(lines, map[string]string{gaugeLine(metricWaiting): ""}),
		"success missing":           editLines(lines, map[string]string{metricSuccessTotal + "{": ""}),
		"finish reason missing":     editLines(lines, map[string]string{successLine("repetition"): ""}),
		"malformed":                 editLines(lines, map[string]string{gaugeLine(metricRunning): gaugeLine(metricRunning) + " not-a-number"}),
		"non-finite":                editLines(lines, map[string]string{gaugeLine(metricWaiting): gaugeLine(metricWaiting) + " +Inf"}),
		"fractional":                editLines(lines, map[string]string{gaugeLine(metricRunning): gaugeLine(metricRunning) + " 0.5"}),
		"out of range":              editLines(lines, map[string]string{successLine("stop"): successLine("stop") + " 1e+17"}),
		"duplicate":                 vllmMetricsBody(testModelID, 0, 0, 0) + successLine("stop") + " 0.0\n",
		"families agree, wrong model": vllmMetricsBody("other.model-v1:0", 0, 0, 0),
		"engine omitted everywhere": editLines(lines, map[string]string{
			gaugeLine(metricRunning): metricRunning + `{model_name="` + testModelID + `"} 0.0`,
			gaugeLine(metricWaiting): metricWaiting + `{model_name="` + testModelID + `"} 0.0`,
			metricSuccessTotal + "{":  "",
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
