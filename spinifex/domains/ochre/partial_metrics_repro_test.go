package ochre

//test:in-package: drives the reaper sweep against a metrics stub that answers
// with only some of the vLLM series.

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type partialMetricsTransport struct{ body string }

func (p partialMetricsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(p.body)),
		Header: http.Header{}, Request: req}, nil
}

// A 200 carrying only one of the three series is not evidence of an idle vLLM
// server, so it must neither clear the hold nor reclaim the endpoint.
func TestPartialMetricsRepro_OneSeriesIsNotIdle(t *testing.T) {
	t.Parallel()
	bodies := map[string]string{
		"waiting only":       "vllm:num_requests_waiting 0\n",
		"running only":       "vllm:num_requests_running 0\n",
		"success total only": "vllm:request_success_total 0\n",
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newReaperFixture(t, ReaperDeps{IdleTTL: time.Minute})
			rec := f.ready(t)
			f.age(t, 10*time.Minute, 10*time.Minute)
			f.svc.deps.HTTPClient = &http.Client{Transport: partialMetricsTransport{body: body}}

			require.NoError(t, f.reaper.sweepOnce(t.Context(), nil))

			got, found := f.current(t)
			t.Logf("body=%q recordFound=%v held=%v terminated=%v", body, found, got.Held(), f.harness.launcher.terminations())
			assert.True(t, found, "a partial scrape must not reclaim the endpoint")
			assert.True(t, got.Held(), "a partial scrape must leave the hold")
			assert.NotContains(t, f.harness.launcher.terminations(), rec.InstanceID)
		})
	}
}
