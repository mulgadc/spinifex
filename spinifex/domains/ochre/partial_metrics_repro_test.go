package ochre

//test:in-package: drives the reaper sweep against a metrics stub that answers
// with only some of the vLLM series.

import (
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type partialMetricsTransport struct {
	body string
	mu   sync.Mutex
	urls []string
}

func (p *partialMetricsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	p.mu.Lock()
	p.urls = append(p.urls, req.URL.String())
	p.mu.Unlock()
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(p.body)),
		Header: http.Header{}, Request: req}, nil
}

func (p *partialMetricsTransport) sent() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.urls)
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
			transport := &partialMetricsTransport{body: body}
			f.svc.deps.HTTPClient = &http.Client{Transport: transport}

			require.NoError(t, f.reaper.sweepOnce(t.Context(), nil))

			got, found := f.current(t)
			t.Logf("body=%q requests=%v recordFound=%v held=%v terminated=%v", body, transport.sent(), found, got.Held(), f.harness.launcher.terminations())
			assert.Equal(t, []string{rec.BaseURL + "/metrics"}, transport.sent(), "the sweep must scrape the primary member's metrics")
			assert.True(t, found, "a partial scrape must not reclaim the endpoint")
			assert.True(t, got.Held(), "a partial scrape must leave the hold")
			assert.NotContains(t, f.harness.launcher.terminations(), rec.InstanceID)
		})
	}
}
