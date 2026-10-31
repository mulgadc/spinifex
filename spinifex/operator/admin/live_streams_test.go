package admin

import (
	"io"
	"net/http"
	"net/url"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStreamLimiter_RefusesPastEitherCapAndFreesOnRelease(t *testing.T) {
	l := newStreamLimiter(2, 3)

	a1, ok, _ := l.acquire("a")
	require.True(t, ok)
	_, ok, _ = l.acquire("a")
	require.True(t, ok)
	_, ok, accountFull := l.acquire("a")
	assert.False(t, ok, "third stream for one account")
	assert.True(t, accountFull)

	_, ok, _ = l.acquire("b")
	require.True(t, ok)
	_, ok, accountFull = l.acquire("c")
	assert.False(t, ok, "fourth stream on the gateway")
	assert.False(t, accountFull)

	a1()
	a1()
	_, ok, _ = l.acquire("c")
	assert.True(t, ok, "a released slot is reusable, once")
	_, ok, _ = l.acquire("c")
	assert.False(t, ok, "a second release of the same slot frees nothing")
}

// A caller past its cap is refused before anything is asked of the sampler,
// and the stream it already holds keeps running.
func TestGetInstanceMetrics_RefusesAStreamPastTheAccountCap(t *testing.T) {
	was := liveStreams
	liveStreams = newStreamLimiter(1, 10)
	t.Cleanup(func() { liveStreams = was })

	d, nc := metricsDeps(t)
	watches := fakeSampler(t, nc, metricsAccount, nil)
	srv := metricsServer(t, d, allowEverything())

	first := postMetrics(t, srv, url.Values{"InstanceId": {metricsInstance}})
	require.Equal(t, http.StatusOK, first.StatusCode)
	<-watches

	second := postMetrics(t, srv, url.Values{"InstanceId": {metricsInstance}})
	body, err := io.ReadAll(second.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), awserrors.ErrorRequestLimitExceeded)
	assert.Empty(t, watches, "a refused stream never reaches the sampler")
}
