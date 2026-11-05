//go:build lambdatestlink

//test:in-package — reads the daemon's unexported slot and owner fixtures
package testlink

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/domains/lambda/execution"
)

// A direct peer's request whose raw payload is 6 MiB + 1 but whose frame is
// within MaxFrameBytes passes the frame check, reaches the real daemon, and
// is refused there before Start with no address or tap taken.
func TestControlDaemon_RawPayloadOverBoundRefusedBeforeStart(t *testing.T) {
	code := []byte("zip bytes")
	srv := codeServer(t, code)
	g := grant(t, code)
	ips, taps := &fakeIPs{}, &fakeTaps{}
	d := g.daemon(&Daemon{IPs: ips, Taps: taps, HTTP: srv.Client(), WorkDir: t.TempDir()})

	req := baseRequest(srv.URL, code)
	req.Grant, req.ImageDigest = g.att.Grant, g.image.Digest
	req.Payload = make([]byte, MaxPayloadBytes+1)
	b, err := json.Marshal(req)
	require.NoError(t, err)
	require.LessOrEqual(t, int64(len(b)), MaxFrameBytes, "the frame alone would admit it")

	name := controlName(t)
	fullServe(t, fullListen(t, name), d.Handle)
	rep, err := fullClient(name).Exchange(context.Background(), b, testCred, time.Now().Add(30*time.Second))
	require.NoError(t, err)
	require.Equal(t, OutcomeRefused, rep.Outcome, rep.Error)
	require.Contains(t, rep.Error, "payload")
	require.Empty(t, ips.allocated)
	require.Empty(t, taps.created)
	require.Equal(t, execution.StateGranted, g.state(t))
}

// With one real attempt started and held in its code fetch, a second request
// through the real control server is refused before Start as busy.
func TestControlDaemon_SecondRequestRefusedWhileAnAttemptIsHeld(t *testing.T) {
	code := []byte("zip bytes")
	fetching, release := make(chan struct{}), make(chan struct{})
	var fetchOnce, releaseOnce sync.Once
	free := func() { releaseOnce.Do(func() { close(release) }) }
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetchOnce.Do(func() { close(fetching) })
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write(code)
	}))
	// Cleanups run last-registered first: free the held fetch before Close.
	t.Cleanup(srv.Close)
	t.Cleanup(free)
	g := grant(t, code)
	ips, taps := &fakeIPs{}, &fakeTaps{}
	d := g.daemon(&Daemon{IPs: ips, Taps: taps, HTTP: srv.Client(), WorkDir: t.TempDir()})
	name := controlName(t)
	fullServe(t, fullListen(t, name), d.Handle)

	first := make(chan error, 1)
	go func() {
		_, err := fullClient(name).Exchange(context.Background(), g.request(srv.URL, code), testCred, time.Now().Add(60*time.Second))
		first <- err
	}()
	// On every path the held attempt is released and its exchange joined.
	t.Cleanup(func() {
		free()
		select {
		case <-first:
		case <-time.After(30 * time.Second):
			t.Error("the held exchange did not return")
		}
	})
	recvBounded(t, fetching)
	require.Equal(t, execution.StateRunning, g.state(t), "the first attempt started")

	rep, err := fullClient(name).Exchange(context.Background(), g.request(srv.URL, code), testCred, time.Now().Add(30*time.Second))
	require.NoError(t, err)
	require.Equal(t, OutcomeRefused, rep.Outcome)
	require.Contains(t, rep.Error, "another")
	require.Empty(t, ips.allocated, "nothing was allocated for either attempt yet")
	require.Empty(t, taps.created)
}
