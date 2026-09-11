//test:in-package — waitForClusterReady and its sleep/deadline seams
//(clusterReadySleep, clusterReadyMaxWait) are unexported, and the tests build a
//Daemon with an unexported natsConn field.

package daemon

import (
	"bytes"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubClusterReadySleep swaps the waitForClusterReady poll delay for fn so tests
// drive the loop instead of waiting out 2s per iteration. Restored via t.Cleanup.
func stubClusterReadySleep(t *testing.T, fn func(time.Duration)) {
	t.Helper()
	prev := clusterReadySleep
	clusterReadySleep = fn
	t.Cleanup(func() { clusterReadySleep = prev })
}

// stubClusterReadyMaxWait shortens the readiness deadline so the timeout branch
// is reachable inside the package test timeout.
func stubClusterReadyMaxWait(t *testing.T, d time.Duration) {
	t.Helper()
	prev := clusterReadyMaxWait
	clusterReadyMaxWait = d
	t.Cleanup(func() { clusterReadyMaxWait = prev })
}

// captureWarnLogs redirects the default logger so the readiness outcome, which
// the function reports only by logging, can be asserted.
func captureWarnLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// closedPort returns an address nothing is listening on.
func closedPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

// TestWaitForClusterReady_ReadyImmediately asserts the loop returns on the first
// pass without sleeping when both dependencies are already reachable.
func TestWaitForClusterReady_ReadyImmediately(t *testing.T) {
	// JetStream, not a bare NATS server: viperblock readiness means the KV the
	// mount claims its volume lease from can answer, and a connection to a
	// server whose JetStream is still starting reports connected regardless.
	_, nc, _ := testutil.StartTestJetStream(t)

	sleeps := 0
	stubClusterReadySleep(t, func(time.Duration) { sleeps++ })
	logs := captureWarnLogs(t)

	d := &Daemon{config: &config.Config{}, natsConn: nc}
	d.waitForClusterReady()

	assert.Equal(t, 0, sleeps, "ready on first check must not sleep")
	assert.NotContains(t, logs.String(), "Cluster readiness timeout")
}

// TestWaitForClusterReady_PredastoreBecomesReady asserts the loop keeps polling
// while predastore is unreachable and returns once it starts serving, rather
// than latching the first not-ready result.
func TestWaitForClusterReady_PredastoreBecomesReady(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)

	d := &Daemon{
		config:   &config.Config{Predastore: config.PredastoreConfig{Host: closedPort(t)}},
		natsConn: nc,
	}

	// Point predastore at the live server from inside the poll delay, so the
	// second iteration observes it without waiting on wall-clock time.
	sleeps := 0
	stubClusterReadySleep(t, func(time.Duration) {
		sleeps++
		d.config.Predastore.Host = u.Host
	})
	logs := captureWarnLogs(t)

	d.waitForClusterReady()

	assert.Equal(t, 1, sleeps, "must poll again after predastore reports not ready")
	assert.NotContains(t, logs.String(), "Cluster readiness timeout")
}

// TestCheckViperblockReady_ConnectedButJetStreamNotServing is the regression
// guard for a guest lost across a cluster update. A TCP connection to NATS says
// nothing about whether JetStream can serve, so readiness used to pass while the
// volume-lease KV was still unavailable, recovery ran into a lease timeout, and
// the instance was parked in error permanently.
func TestCheckViperblockReady_ConnectedButJetStreamNotServing(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	require.True(t, nc.IsConnected(), "the connection itself must be up, or this proves nothing")

	d := &Daemon{config: &config.Config{}, natsConn: nc}

	assert.False(t, d.checkViperblockReady(),
		"connected to a server with no JetStream is not ready: the lease KV cannot answer")
}

// TestCheckViperblockReady_JetStreamServing is the other half: once JetStream
// answers, readiness must pass rather than holding startup open.
func TestCheckViperblockReady_JetStreamServing(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)

	d := &Daemon{config: &config.Config{}, natsConn: nc}

	assert.True(t, d.checkViperblockReady())
}

// TestWaitForClusterReady_TimesOut asserts the deadline branch: with viperblock
// permanently unreachable the loop gives up at maxWait, logs the warning and
// returns so recovery proceeds anyway, instead of blocking startup forever.
func TestWaitForClusterReady_TimesOut(t *testing.T) {
	stubClusterReadyMaxWait(t, 20*time.Millisecond)

	sleeps := 0
	stubClusterReadySleep(t, func(time.Duration) {
		sleeps++
		time.Sleep(5 * time.Millisecond)
	})
	logs := captureWarnLogs(t)

	// natsConn nil means viperblock never reports ready.
	d := &Daemon{config: &config.Config{}}
	d.waitForClusterReady()

	assert.Positive(t, sleeps, "must poll before timing out")
	assert.Contains(t, logs.String(), "Cluster readiness timeout, proceeding with recovery anyway")
}
