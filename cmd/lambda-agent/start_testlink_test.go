//go:build lambdatestlink

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	task    string
	sealed  bool
}

// testlink builds a guest whose fw_cfg also carries a controlled package,
// installed into a temp dir without mounting anything.
func (b *bootstrapStarts) testlink(t *testing.T, entries map[string][]byte) testlink {
	t.Helper()
	zipBytes := testZip(t, map[string]string{"bootstrap": "#!/bin/sh\n", "lib/data.txt": "data"})
	disk := filepath.Join(t.TempDir(), "disk")
	require.NoError(t, os.WriteFile(disk, zipBytes, 0o600))
	sum := sha256.Sum256(zipBytes)
	entries[entryPackageSize] = []byte(strconv.Itoa(len(zipBytes)))
	entries[entryPackageSHA256] = []byte(hex.EncodeToString(sum[:]))
	g := newGuest(t, entries)
	b.started = make(chan struct{}, 1)
	b.task = t.TempDir()
	return testlink{
		startup: g.startup(),
		pkg: taskPackage{
			device: disk, dir: b.task,
			loadDriver: func() error { return nil },
			mountDir:   func(string) error { return nil },
			sealDir:    func(string) error { b.sealed = true; return nil },
		},
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
	done := runTestlinkAsync(ctx, b.testlink(t, h.entries()))

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
	assert.True(t, b.sealed, "task dir not made read-only")
	assert.FileExists(t, filepath.Join(b.task, "lib", "data.txt"))
}

func TestTestlinkShutdownBeforeInvocationNeverStartsBootstrap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h := newHost(t)
	h.accept(ctx)
	var b bootstrapStarts
	done := runTestlinkAsync(ctx, b.testlink(t, h.entries()))

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

	res := wait(t, runTestlinkAsync(ctx, b.testlink(t, entries)), "agent to stop")
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
	done := runTestlinkAsync(ctx, b.testlink(t, h.entries()))

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

// A package that fails its check is refused after authentication, and the
// bootstrap never starts.
func TestTestlinkBadPackageNeverStartsBootstrap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h := newHost(t)
	h.accept(ctx)
	var b bootstrapStarts
	tl := b.testlink(t, h.entries())
	tl.pkg.device = filepath.Join(t.TempDir(), "other")
	require.NoError(t, os.WriteFile(tl.pkg.device, testZip(t, map[string]string{"bootstrap": "x"}), 0o600))
	done := runTestlinkAsync(ctx, tl)

	conn := h.guest(t)
	defer conn.Close()
	require.NoError(t, hostlink.NewTestStream(conn).HostSend(hostlink.Request{RequestID: "r-1"}))
	res := wait(t, done, "agent to stop")
	assert.Equal(t, exitRefused, res.code)
	assert.ErrorContains(t, res.err, "install package")
	assert.Zero(t, b.n)
	assert.False(t, b.sealed)
}

// Missing package values still seal fw_cfg, and the agent refuses before
// waiting for any frame.
func TestTestlinkMissingPackageSpecRefuses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h := newHost(t)
	h.accept(ctx)
	var b bootstrapStarts
	tl := b.testlink(t, h.entries())
	require.NoError(t, os.RemoveAll(filepath.Join(tl.fwCfgDir, entryPackageSHA256)))

	res := wait(t, runTestlinkAsync(ctx, tl), "agent to stop")
	assert.Equal(t, exitRefused, res.code)
	assert.ErrorContains(t, res.err, "read "+entryPackageSHA256)
	assert.Zero(t, b.n)
	_, err := os.Stat(tl.fwCfgRoot)
	assert.ErrorIs(t, err, os.ErrNotExist, "fw_cfg not sealed")
}
