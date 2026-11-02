//go:build lambdatestlink

package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	lambdav1 "github.com/mulgadc/spinifex/contracts/lambda/v1"
	"github.com/mulgadc/spinifex/spinifex/agents/lambda/hostlink"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/lambda/guestconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const helperEnv = "LAMBDA_AGENT_TEST_BOOTSTRAP"

// TestHelperBootstrap is the fake function bootstrap, run as a subprocess: it
// echoes one invocation through the Runtime API, then waits for the next.
func TestHelperBootstrap(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		return
	}
	base := "http://" + os.Getenv("AWS_LAMBDA_RUNTIME_API") + "/2018-06-01/runtime/invocation/"
	resp, err := http.Get(base + "next")
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	resp.Body.Close()
	id := resp.Header.Get("Lambda-Runtime-Aws-Request-Id")
	post, err := http.Post(base+id+"/response", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	post.Body.Close()
	_, _ = http.Get(base + "next")
}

type bootstrapStarts struct {
	n       int
	started chan struct{}
}

func (b *bootstrapStarts) testlink(g *guest) testlink {
	b.started = make(chan struct{}, 1)
	return testlink{
		startup:     g.startup(),
		runtimeAddr: "127.0.0.1:0",
		startBootstrap: func(ctx context.Context, env []string) (*exec.Cmd, error) {
			b.n++
			b.started <- struct{}{}
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHelperBootstrap$")
			cmd.Env = append(env, helperEnv+"=1")
			return cmd, cmd.Start()
		},
	}
}

func runTestlinkAsync(ctx context.Context, tl testlink) <-chan result {
	out := make(chan result, 1)
	go func() {
		code, err := tl.run(ctx)
		out <- result{code, err}
	}()
	return out
}

type result struct {
	code int
	err  error
}

func TestTestlinkServesOneInvocationThenStops(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h := newHost(t)
	h.accept(ctx)
	var b bootstrapStarts
	done := runTestlinkAsync(ctx, b.testlink(newGuest(t, h.entries())))

	conn := h.guest(t)
	defer conn.Close()
	select {
	case <-b.started:
		t.Fatal("bootstrap started before the host sent a frame")
	case <-time.After(100 * time.Millisecond):
	}
	hs := hostlink.NewTestStream(conn)
	req := hostlink.Request{GrantID: "g-1", AttemptID: "a-1", RequestID: "r-1", DeadlineMs: time.Now().Add(5 * time.Second).UnixMilli(), Payload: []byte(`{"a":40,"b":2}`)}
	require.NoError(t, hs.HostSend(req))
	got, err := hs.HostResult(ctx)
	require.NoError(t, err)
	assert.Equal(t, hostlink.Response{GrantID: "g-1", AttemptID: "a-1", RequestID: "r-1", Payload: req.Payload}, got)

	// A second invocation on the same VM is never served. The agent may
	// already have closed the connection, so a failed send is a refusal too.
	second := req
	second.RequestID = "r-2"
	sendErr := hs.HostSend(second)
	res := wait(t, done, "agent to stop after one invocation")
	assert.Equal(t, exitClosed, res.code)
	assert.NoError(t, res.err)
	if sendErr == nil {
		_, err = hs.HostResult(ctx)
		assert.Error(t, err, "agent answered a second invocation")
	}
	assert.Equal(t, 1, b.n)
}

func TestTestlinkShutdownBeforeInvocationNeverStartsBootstrap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h := newHost(t)
	h.accept(ctx)
	var b bootstrapStarts
	done := runTestlinkAsync(ctx, b.testlink(newGuest(t, h.entries())))

	conn := h.guest(t)
	defer conn.Close()
	require.NoError(t, hostlink.NewTestStream(conn).HostShutdown())
	res := wait(t, done, "agent to stop")
	assert.Equal(t, exitClosed, res.code)
	assert.NoError(t, res.err)
	assert.Zero(t, b.n)
}

// The guest's handshake completes, but the host pins another generation's
// guest; the refusal arrives on first read and the bootstrap never starts.
func TestTestlinkHostRefusalNeverStartsBootstrap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h := newHost(t)
	h.accept(ctx)
	other, err := guestconn.NewGeneration(scope, time.Hour)
	require.NoError(t, err)
	entries := h.entries()
	wrong := other.GuestEntries(h.ln.Addr().String())
	entries[lambdav1.EntryGuestKey], entries[lambdav1.EntryGuestCert] = wrong[lambdav1.EntryGuestKey], wrong[lambdav1.EntryGuestCert]
	var b bootstrapStarts

	res := wait(t, runTestlinkAsync(ctx, b.testlink(newGuest(t, entries))), "agent to stop")
	assert.Equal(t, exitLink, res.code)
	require.Error(t, res.err)
	assert.Contains(t, res.err.Error(), "bad certificate")
	assert.Zero(t, b.n)
}

func TestTestlinkHostCloseBeforeFrameNeverStartsBootstrap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h := newHost(t)
	h.accept(ctx)
	var b bootstrapStarts
	done := runTestlinkAsync(ctx, b.testlink(newGuest(t, h.entries())))

	require.NoError(t, h.guest(t).Close())
	res := wait(t, done, "agent to stop")
	assert.Equal(t, exitLink, res.code)
	assert.ErrorContains(t, res.err, "first frame from host")
	assert.Zero(t, b.n)
}

// A cancelled context is reported before the buffered request is delivered,
// and the request stays undelivered.
func TestOneInvocationHonoursCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	o := &oneInvocation{first: &hostlink.Request{RequestID: "r-1"}}
	_, err := o.Next(ctx)
	require.ErrorIs(t, err, context.Canceled)

	req, err := o.Next(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "r-1", req.RequestID)
}
