package admin

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	telemetryv1 "github.com/mulgadc/spinifex/contracts/telemetry/v1"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	metricsAccount  = "000000000002"
	metricsInstance = "i-0123456789abcdef0"
	metricsRegion   = "ap-southeast-2"
)

// recordingAuthorizer records every check and allows only what allow accepts.
type recordingAuthorizer struct {
	mu    sync.Mutex
	calls []authzResourceCall
	allow func(action string, resources []string) bool
}

type authzResourceCall struct {
	service, action string
	resources       []string
}

func (a *recordingAuthorizer) authorize(service, action string, resources []string, _ iampolicy.ConditionKeys) error {
	a.mu.Lock()
	a.calls = append(a.calls, authzResourceCall{service, action, resources})
	a.mu.Unlock()
	if a.allow != nil && !a.allow(action, resources) {
		return errors.New(awserrors.ErrorAccessDenied)
	}
	return nil
}

func allowEverything() *recordingAuthorizer {
	return &recordingAuthorizer{allow: func(string, []string) bool { return true }}
}

// fakeSampler answers watches for one account's instance with a fixed ring,
// as the owning node's collector does, and counts them.
func fakeSampler(t *testing.T, nc *nats.Conn, account string, ring []telemetryv1.LiveSample) chan telemetryv1.LiveWatch {
	t.Helper()
	watches := make(chan telemetryv1.LiveWatch, 16)
	sub, err := nc.Subscribe(telemetryv1.LiveWatchSubject(account, metricsInstance), func(msg *nats.Msg) {
		var watch telemetryv1.LiveWatch
		_ = json.Unmarshal(msg.Data, &watch)
		watches <- watch
		if msg.Reply == "" {
			return
		}
		var samples []telemetryv1.LiveSample
		for _, s := range ring {
			if s.TimeMillis > watch.SinceMillis {
				samples = append(samples, s)
			}
		}
		data, _ := json.Marshal(telemetryv1.LiveRing{Samples: samples})
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return watches
}

// metricsServer serves the dispatcher over real HTTP as the caller's account,
// so the response can be read while it streams.
func metricsServer(t *testing.T, d Deps, authz *recordingAuthorizer) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := d.dispatch(w, dispatch.Invocation{
			Request:   r,
			AccountID: metricsAccount,
			Service:   ServiceName,
			Region:    metricsRegion,
			Authorize: authz.authorize,
		})
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, err.Error())
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func postMetrics(t *testing.T, srv *httptest.Server, params url.Values) *http.Response {
	t.Helper()
	params.Set("Action", GetInstanceMetricsAction)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL,
		strings.NewReader(params.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// readSamples reads n sample events off an event stream.
func readSamples(t *testing.T, body io.Reader, n int) []telemetryv1.LiveSample {
	t.Helper()
	scanner := bufio.NewScanner(body)
	var got []telemetryv1.LiveSample
	for len(got) < n && scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok {
			continue
		}
		var s telemetryv1.LiveSample
		require.NoError(t, json.Unmarshal([]byte(data), &s))
		got = append(got, s)
	}
	require.Len(t, got, n)
	return got
}

func metricsDeps(t *testing.T) (Deps, *nats.Conn) {
	t.Helper()
	_, nc := testutil.StartTestNATS(t)
	return Deps{NATS: nc}, nc
}

func TestGetInstanceMetrics_StreamsTheRingThenLiveSamplesOnce(t *testing.T) {
	d, nc := metricsDeps(t)
	ring := []telemetryv1.LiveSample{{TimeMillis: 1000, CPUPercent: 1}, {TimeMillis: 1250, CPUPercent: 2}, {TimeMillis: 1500, CPUPercent: 3}}
	watches := fakeSampler(t, nc, metricsAccount, ring)
	srv := metricsServer(t, d, allowEverything())

	resp := postMetrics(t, srv, url.Values{"InstanceId": {metricsInstance}, "Since": {"1000"}})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	assert.Equal(t, int64(1000), (<-watches).SinceMillis, "the watch asks for what the viewer lacks")

	// 1500 arrives again live, as it would if published while the ring was read.
	subject := telemetryv1.LiveSampleSubject(metricsAccount, metricsInstance)
	for _, s := range []telemetryv1.LiveSample{{TimeMillis: 1500, CPUPercent: 3}, {TimeMillis: 1750, CPUPercent: 4}} {
		data, err := json.Marshal(s)
		require.NoError(t, err)
		require.NoError(t, nc.Publish(subject, data))
	}

	got := readSamples(t, resp.Body, 3)
	assert.Equal(t, []int64{1250, 1500, 1750}, []int64{got[0].TimeMillis, got[1].TimeMillis, got[2].TimeMillis})
}

func TestGetInstanceMetrics_RenewsTheWatchAndEndsTheStream(t *testing.T) {
	shorten(t, &liveWatchInterval, 50*time.Millisecond)
	shorten(t, &liveStreamDuration, 300*time.Millisecond)
	d, nc := metricsDeps(t)
	watches := fakeSampler(t, nc, metricsAccount, nil)
	srv := metricsServer(t, d, allowEverything())

	start := time.Now()
	resp := postMetrics(t, srv, url.Values{"InstanceId": {metricsInstance}})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.GreaterOrEqual(t, time.Since(start), 300*time.Millisecond, "the stream lasts its segment")
	assert.GreaterOrEqual(t, len(watches), 3, "the first watch plus renewals while open")
}

func TestGetInstanceMetrics_AnotherAccountsInstanceIsNotFound(t *testing.T) {
	d, nc := metricsDeps(t)
	// Running, but in a different account from the caller's.
	fakeSampler(t, nc, "000000000003", nil)
	srv := metricsServer(t, d, allowEverything())

	resp := postMetrics(t, srv, url.Values{"InstanceId": {metricsInstance}})
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, string(body), awserrors.ErrorInvalidInstanceIDNotFound)
}

// The only check is against the instance's ARN, never "*", and a refusal there
// ends the request before anything is asked of the sampler.
func TestGetInstanceMetrics_AuthorizesAgainstTheInstanceARNOnly(t *testing.T) {
	instanceARN := "arn:aws:ec2:" + metricsRegion + ":" + metricsAccount + ":instance/" + metricsInstance

	for _, allowed := range []bool{true, false} {
		shorten(t, &liveStreamDuration, 50*time.Millisecond)
		d, nc := metricsDeps(t)
		watches := fakeSampler(t, nc, metricsAccount, nil)
		authz := &recordingAuthorizer{allow: func(string, []string) bool { return allowed }}
		srv := metricsServer(t, d, authz)

		resp := postMetrics(t, srv, url.Values{"InstanceId": {metricsInstance}})
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		assert.Equal(t, []authzResourceCall{{ServiceName, GetInstanceMetricsAction, []string{instanceARN}}}, authz.calls)
		if allowed {
			assert.Equal(t, http.StatusOK, resp.StatusCode, string(body))
			continue
		}
		assert.Contains(t, string(body), awserrors.ErrorAccessDenied)
		assert.Empty(t, watches, "a refused caller never reaches the sampler")
	}
}

func TestGetInstanceMetrics_RejectsBadParameters(t *testing.T) {
	d, _ := metricsDeps(t)
	srv := metricsServer(t, d, allowEverything())

	for _, tc := range []struct {
		params url.Values
		code   string
	}{
		{url.Values{}, awserrors.ErrorMissingParameter},
		{url.Values{"InstanceId": {">"}}, awserrors.ErrorInvalidInstanceIDMalformed},
		{url.Values{"InstanceId": {"i-0123.abc"}}, awserrors.ErrorInvalidInstanceIDMalformed},
		{url.Values{"InstanceId": {metricsInstance}, "Since": {"soon"}}, awserrors.ErrorInvalidParameterValue},
	} {
		resp := postMetrics(t, srv, tc.params)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Contains(t, string(body), tc.code, "%v", tc.params)
	}
}

func TestGetInstanceMetrics_NoNATSIsServerInternal(t *testing.T) {
	srv := metricsServer(t, Deps{}, allowEverything())

	resp := postMetrics(t, srv, url.Values{"InstanceId": {metricsInstance}})
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), awserrors.ErrorServerInternal)
}

func shorten(t *testing.T, setting *time.Duration, d time.Duration) {
	t.Helper()
	was := *setting
	*setting = d
	t.Cleanup(func() { *setting = was })
}
