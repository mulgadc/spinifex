//go:build lambdatestlink

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	lambdav1 "github.com/mulgadc/spinifex/contracts/lambda/v1"
	"github.com/mulgadc/spinifex/spinifex/agents/lambda/hostlink"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/lambda/guestconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const helperEnv = "LAMBDA_AGENT_TEST_BOOTSTRAP"

// helperHandlerOut names the file the helper bootstrap writes its _HANDLER to.
const helperHandlerOut = "LAMBDA_AGENT_TEST_HANDLER_OUT"

// TestHelperBootstrap is the fake function bootstrap, run as a subprocess: it
// echoes one invocation through the Runtime API, then waits for the next.
func TestHelperBootstrap(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		return
	}
	if out := os.Getenv(helperHandlerOut); out != "" {
		require.NoError(t, os.WriteFile(out, []byte(os.Getenv("_HANDLER")), 0o600))
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
	if os.Getenv(helperExitAfterPost) == "1" {
		return
	}
	_, _ = http.Get(base + "next")
}

// helperExitAfterPost makes the helper bootstrap exit once its response is posted.
const helperExitAfterPost = "LAMBDA_AGENT_TEST_EXIT_AFTER_POST"

// testGrantEntry grants g-1 for attempt a-1 on the fixture scope's env-1/1.
const testGrantEntry = "g-1\na-1\nenv-1\n1\n"

// testHandler is the handler the fixture's host configures.
const testHandler = "bootstrap.fixture"

type bootstrapStarts struct {
	n       int
	started chan struct{}
	task    string
	sealed  bool
	// handlerOut receives the _HANDLER the bootstrap process itself saw.
	handlerOut string
	caBundle   string
	// env is the environment the agent gave the bootstrap.
	env []string
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
	if _, ok := entries[entryHandler]; !ok {
		entries[entryHandler] = []byte(testHandler)
	}
	if _, ok := entries[entryGrant]; !ok {
		entries[entryGrant] = []byte(testGrantEntry)
	}
	g := newGuest(t, entries)
	b.started = make(chan struct{}, 1)
	b.task = t.TempDir()
	b.handlerOut = filepath.Join(t.TempDir(), "handler")
	b.caBundle = filepath.Join(t.TempDir(), "sdk", "ca-bundle.pem")
	return testlink{
		startup: g.startup(),
		pkg: taskPackage{
			device: disk, dir: b.task,
			loadDriver: func() error { return nil },
			mountDir:   func(string) error { return nil },
			sealDir:    func(string) error { b.sealed = true; return nil },
		},
		runtimeAddr:  "127.0.0.1:0",
		caBundlePath: b.caBundle,
		systemCAPath: filepath.Join(t.TempDir(), "system-ca.pem"),
		startBootstrap: func(ctx context.Context, env []string) (*exec.Cmd, error) {
			b.n++
			b.env = env
			b.started <- struct{}{}
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHelperBootstrap$")
			cmd.Env = append(env, helperEnv+"=1", helperHandlerOut+"="+b.handlerOut)
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
	req := hostlink.Request{Credential: &testCredential, GrantID: "g-1", AttemptID: "a-1", RequestID: "r-1", DeadlineMs: time.Now().Add(5 * time.Second).UnixMilli(), Payload: []byte(`{"a":40,"b":2}`)}
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
	seen, err := os.ReadFile(b.handlerOut)
	require.NoError(t, err, "bootstrap process did not report its handler")
	assert.Equal(t, testHandler, string(seen), "bootstrap process not given the configured handler")
	assert.True(t, b.sealed, "task dir not made read-only")
	assert.FileExists(t, filepath.Join(b.task, "lib", "data.txt"))
	assert.ElementsMatch(t, []string{
		"AWS_LAMBDA_RUNTIME_API=" + envValue(b.env, "AWS_LAMBDA_RUNTIME_API"),
		"_HANDLER=" + testHandler,
		"LAMBDA_TASK_ROOT=" + taskRoot,
		"PATH=/bin:/sbin:/usr/bin",
		"AWS_ACCESS_KEY_ID=" + testCredential.AccessKeyID,
		"AWS_SECRET_ACCESS_KEY=" + testCredential.SecretAccessKey,
		"AWS_SESSION_TOKEN=" + testCredential.SessionToken,
		"AWS_REGION=ap-southeast-2",
		"AWS_DEFAULT_REGION=ap-southeast-2",
	}, b.env, "the bootstrap gets exactly the runtime and credential variables")
	assert.NotEmpty(t, envValue(b.env, "AWS_LAMBDA_RUNTIME_API"))
}

func TestTestlinkProvidesSDKSettingsToBootstrap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	h := newHost(t)
	h.accept(ctx)
	entries := h.entries()
	entries[sdkEndpointEntry] = []byte(server.URL)
	entries[sdkCAEntry] = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	var b bootstrapStarts
	done := runTestlinkAsync(ctx, b.testlink(t, entries))
	conn := h.guest(t)
	defer conn.Close()
	hs := hostlink.NewTestStream(conn)
	req := hostlink.Request{Credential: &testCredential, GrantID: "g-1", AttemptID: "a-1", RequestID: "r-1", DeadlineMs: time.Now().Add(5 * time.Second).UnixMilli(), Payload: []byte(`{}`)}
	require.NoError(t, hs.HostSend(req))
	_, err := hs.HostResult(ctx)
	require.NoError(t, err)
	res := wait(t, done, "agent to stop")
	require.NoError(t, res.err)
	assert.Equal(t, server.URL, envValue(b.env, "AWS_ENDPOINT_URL"))
	assert.Equal(t, b.caBundle, envValue(b.env, "AWS_CA_BUNDLE"))
	assert.Equal(t, "ap-southeast-2", envValue(b.env, "AWS_REGION"))
	assert.Equal(t, "ap-southeast-2", envValue(b.env, "AWS_DEFAULT_REGION"))
	ca, err := os.ReadFile(b.caBundle)
	require.NoError(t, err)
	assert.Contains(t, string(ca), string(entries[sdkCAEntry]))
	info, err := os.Stat(b.caBundle)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())
}

func TestTestlinkProvidesFunctionEnvironment(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h := newHost(t)
	h.accept(ctx)
	var b bootstrapStarts
	done := runTestlinkAsync(ctx, b.testlink(t, h.entries()))
	conn := h.guest(t)
	defer conn.Close()
	hs := hostlink.NewTestStream(conn)
	cred := testCredential
	cred.Environment = map[string]string{"GREETING": "hello $WORLD", "PATH": "/my/bin"}
	req := hostlink.Request{Credential: &cred, GrantID: "g-1", AttemptID: "a-1", RequestID: "r-1", DeadlineMs: time.Now().Add(5 * time.Second).UnixMilli(), Payload: []byte(`{}`)}
	require.NoError(t, hs.HostSend(req))
	_, err := hs.HostResult(ctx)
	require.NoError(t, err)
	require.NoError(t, wait(t, done, "agent to stop").err)
	assert.Equal(t, "hello $WORLD", envValue(b.env, "GREETING"))
	assert.Equal(t, "/my/bin", envValue(b.env, "PATH"))
	assert.Equal(t, testCredential.SecretAccessKey, envValue(b.env, "AWS_SECRET_ACCESS_KEY"))
}

func TestTestlinkRejectsReservedFunctionEnvironment(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h := newHost(t)
	h.accept(ctx)
	var b bootstrapStarts
	done := runTestlinkAsync(ctx, b.testlink(t, h.entries()))
	conn := h.guest(t)
	defer conn.Close()
	cred := testCredential
	cred.Environment = map[string]string{"AWS_SECRET_ACCESS_KEY": "forged"}
	req := hostlink.Request{Credential: &cred, GrantID: "g-1", AttemptID: "a-1", RequestID: "r-1", DeadlineMs: time.Now().Add(5 * time.Second).UnixMilli(), Payload: []byte(`{}`)}
	require.NoError(t, hostlink.NewTestStream(conn).HostSend(req))
	result := wait(t, done, "agent to refuse the environment")
	assert.Equal(t, exitRefused, result.code)
	assert.Error(t, result.err)
	assert.NotContains(t, result.err.Error(), "forged")
	assert.Zero(t, b.n)
}

var testCredential = hostlink.Credential{AccessKeyID: "ASIAGUESTKEY", SecretAccessKey: "guest-secret", SessionToken: "guest-token"}

func envValue(env []string, name string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, name+"="); ok {
			return v
		}
	}
	return ""
}

// A first frame without a complete credential installs and runs nothing.
func TestTestlinkFrameWithoutCredentialNeverStartsBootstrap(t *testing.T) {
	for name, cred := range map[string]*hostlink.Credential{
		"none":       nil,
		"incomplete": {AccessKeyID: "ASIAGUESTKEY"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			h := newHost(t)
			h.accept(ctx)
			var b bootstrapStarts
			done := runTestlinkAsync(ctx, b.testlink(t, h.entries()))
			conn := h.guest(t)
			defer conn.Close()
			require.NoError(t, hostlink.NewTestStream(conn).HostSend(hostlink.Request{Credential: cred, GrantID: "g-1", AttemptID: "a-1",
				RequestID: "r-1", DeadlineMs: time.Now().Add(5 * time.Second).UnixMilli(), Payload: []byte(`{}`)}))

			res := wait(t, done, "agent to stop")
			assert.Equal(t, exitRefused, res.code)
			assert.ErrorContains(t, res.err, "no execution credential")
			assert.Zero(t, b.n)
			assert.False(t, b.sealed)
			assert.NoFileExists(t, filepath.Join(b.task, "bootstrap"), "package installed")
		})
	}
}

// A bootstrap that posts its response and exits at once does not cost the
// host the result; the agent then waits for the host's shutdown.
func TestTestlinkBootstrapExitingAfterResponseStillDelivers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h := newHost(t)
	h.accept(ctx)
	var b bootstrapStarts
	tl := b.testlink(t, h.entries())
	start := tl.startBootstrap
	tl.startBootstrap = func(ctx context.Context, env []string) (*exec.Cmd, error) {
		return start(ctx, append(env, helperExitAfterPost+"=1"))
	}
	done := runTestlinkAsync(ctx, tl)

	conn := h.guest(t)
	defer conn.Close()
	hs := hostlink.NewTestStream(conn)
	payload := bytes.Repeat([]byte("0123456789"), 300_000)
	req := hostlink.Request{Credential: &testCredential, GrantID: "g-1", AttemptID: "a-1", RequestID: "r-1", DeadlineMs: time.Now().Add(5 * time.Second).UnixMilli(), Payload: payload}
	require.NoError(t, hs.HostSend(req))
	got, err := hs.HostResult(ctx)
	require.NoError(t, err)
	assert.Equal(t, hostlink.Response{GrantID: "g-1", AttemptID: "a-1", RequestID: "r-1", Payload: payload}, got)

	require.NoError(t, hs.HostShutdown())
	res := wait(t, done, "agent to stop after the host's shutdown")
	assert.Equal(t, exitClosed, res.code)
	assert.NoError(t, res.err)
}

// The agent stays up for the host's shutdown rather than exiting as soon as
// its result is written, and never dispatches a stray frame.
func TestOneInvocationAfterResult(t *testing.T) {
	first := hostlink.Request{RequestID: "r-1", DeadlineMs: time.Now().Add(time.Minute).UnixMilli()}
	cases := map[string]struct {
		next func(context.Context) (hostlink.Request, error)
		want error
	}{
		"shutdown frame": {func(context.Context) (hostlink.Request, error) { return hostlink.Request{}, hostlink.ErrShutdown }, hostlink.ErrShutdown},
		"eof":            {func(context.Context) (hostlink.Request, error) { return hostlink.Request{}, io.EOF }, hostlink.ErrShutdown},
		"a second request is not dispatched": {func(context.Context) (hostlink.Request, error) {
			return hostlink.Request{RequestID: "r-2"}, nil
		}, hostlink.ErrShutdown},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			o := &oneInvocation{Link: stubLink{tc.next}, first: &first}
			_, err := o.Next(context.Background())
			require.NoError(t, err)
			req, err := o.Next(context.Background())
			require.ErrorIs(t, err, tc.want)
			assert.Empty(t, req.RequestID)
		})
	}
	t.Run("host never sends shutdown", func(t *testing.T) {
		past := first
		past.DeadlineMs = time.Now().Add(-time.Second).UnixMilli()
		o := &oneInvocation{Link: stubLink{func(ctx context.Context) (hostlink.Request, error) {
			<-ctx.Done()
			return hostlink.Request{}, ctx.Err()
		}}, first: &past}
		_, err := o.Next(context.Background())
		require.NoError(t, err)
		start := time.Now()
		_, err = o.Next(context.Background())
		require.ErrorIs(t, err, hostlink.ErrShutdown)
		assert.GreaterOrEqual(t, time.Since(start), shutdownGrace-100*time.Millisecond, "waited the grace")
	})
	t.Run("parent cancel is not a shutdown", func(t *testing.T) {
		o := &oneInvocation{Link: stubLink{func(ctx context.Context) (hostlink.Request, error) {
			<-ctx.Done()
			return hostlink.Request{}, ctx.Err()
		}}, first: &first}
		_, _ = o.Next(context.Background())
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := o.Next(ctx)
		require.ErrorIs(t, err, context.Canceled)
	})
}

type stubLink struct {
	next func(context.Context) (hostlink.Request, error)
}

var _ hostlink.Link = stubLink{}

func (l stubLink) Next(ctx context.Context) (hostlink.Request, error) { return l.next(ctx) }

func (stubLink) Complete(context.Context, hostlink.Response) error { return nil }

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
	require.NoError(t, hostlink.NewTestStream(conn).HostSend(hostlink.Request{Credential: &testCredential, GrantID: "g-1", AttemptID: "a-1", RequestID: "r-1",
		DeadlineMs: time.Now().Add(5 * time.Second).UnixMilli()}))
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

// A request that does not carry this VM's grant, or a grant for another
// environment generation than the one authenticated, installs and runs
// nothing of the tenant's.
func TestTestlinkUnboundRequestNeverStartsBootstrap(t *testing.T) {
	cases := map[string]struct {
		grant            string
		grantID, attempt string
		deadlineMs       int64
		want             string
	}{
		"frame with no deadline":      {testGrantEntry, "g-1", "a-1", -1, "no deadline"},
		"frame with expired deadline": {testGrantEntry, "g-1", "a-1", time.Now().Add(-time.Second).UnixMilli(), "deadline already passed"},
		"frame for another grant":     {testGrantEntry, "g-2", "a-1", 0, "does not carry this VM's grant"},
		"frame for another attempt":   {testGrantEntry, "g-1", "a-2", 0, "does not carry this VM's grant"},
		"frame with no grant":         {testGrantEntry, "", "", 0, "does not carry this VM's grant"},
		"grant for another env":       {"g-1\na-1\nenv-2\n1\n", "g-1", "a-1", 0, "not for the authenticated environment"},
		"grant for another gen":       {"g-1\na-1\nenv-1\n2\n", "g-1", "a-1", 0, "not for the authenticated environment"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			h := newHost(t)
			h.accept(ctx)
			var b bootstrapStarts
			entries := h.entries()
			entries[entryGrant] = []byte(tc.grant)
			done := runTestlinkAsync(ctx, b.testlink(t, entries))
			conn := h.guest(t)
			defer conn.Close()
			deadline := time.Now().Add(5 * time.Second).UnixMilli()
			switch {
			case tc.deadlineMs < 0:
				deadline = 0
			case tc.deadlineMs > 0:
				deadline = tc.deadlineMs
			}
			req := hostlink.Request{Credential: &testCredential, GrantID: tc.grantID, AttemptID: tc.attempt, RequestID: "r-1",
				DeadlineMs: deadline, Payload: []byte(`{}`)}
			require.NoError(t, hostlink.NewTestStream(conn).HostSend(req))

			res := wait(t, done, "agent to stop")
			assert.Equal(t, exitRefused, res.code)
			assert.ErrorContains(t, res.err, tc.want)
			assert.Zero(t, b.n)
			assert.False(t, b.sealed)
			assert.NoFileExists(t, filepath.Join(b.task, "bootstrap"), "package installed")
		})
	}
}

// A missing or malformed grant entry refuses before any frame is awaited.
func TestTestlinkBadGrantEntryRefuses(t *testing.T) {
	cases := map[string]*string{
		"missing":       nil,
		"too few lines": ptr("g-1\na-1\nenv-1\n"),
		"generation 0":  ptr("g-1\na-1\nenv-1\n0\n"),
		"empty grant":   ptr("\na-1\nenv-1\n1\n"),
	}
	for name, grant := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			h := newHost(t)
			h.accept(ctx)
			var b bootstrapStarts
			entries := h.entries()
			if grant != nil {
				entries[entryGrant] = []byte(*grant)
			}
			tl := b.testlink(t, entries)
			if grant == nil {
				require.NoError(t, os.RemoveAll(filepath.Join(tl.fwCfgDir, entryGrant)))
			}
			res := wait(t, runTestlinkAsync(ctx, tl), "agent to stop")
			assert.Equal(t, exitRefused, res.code)
			assert.ErrorContains(t, res.err, entryGrant)
			assert.Zero(t, b.n)
		})
	}
}

func ptr(s string) *string { return &s }
