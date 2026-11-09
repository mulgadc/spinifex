//go:build lambdatestlink && lambdatestlinkseam

package awsgw

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/agents/lambda/agent"
	"github.com/mulgadc/spinifex/spinifex/agents/lambda/hostlink"
	"github.com/mulgadc/spinifex/spinifex/agents/lambda/runtimeapi"
	lambdaawsapi "github.com/mulgadc/spinifex/spinifex/domains/lambda/awsapi"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/codestore"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/execution"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/function"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/lambda/testlink"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// seamCredential stands in for a presigned credential; it is not a real key.
const seamCredential = "AKIASYNTHETICSEAMKEY"

var seamCode = []byte("synthetic function code")

// seamHandler is the configured handler of the seam's function.
const seamHandler = "bootstrap.seam"

// seamFunction serves one Active function "f" whose stored code is seamCode.
type seamFunction struct {
	timeout   int64
	envCipher string
	role      string
}

var _ function.Client = seamFunction{}

func (s seamFunction) Get(_ context.Context, account, region, name string) (function.Record, uint64, error) {
	if name != "f" {
		return function.Record{}, 0, function.ErrNotFound
	}
	sum := sha256.Sum256(seamCode)
	return function.Record{FunctionID: "fn1", AccountID: account, Region: region, FunctionName: name,
		Role: s.role, Runtime: "provided.al2023", Handler: seamHandler, EnvCipher: s.envCipher, Timeout: s.timeout, MemorySize: 128, State: function.StateActive,
		Code: function.Code{RevisionID: "r1", State: function.CodeStored, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(seamCode))},
	}, 1, nil
}

func (seamFunction) Create(context.Context, string, string, function.Record) (uint64, error) {
	return 0, errors.New("unused")
}

func (seamFunction) CodeStored(context.Context, string, string, string, uint64, string, int64) (function.Record, uint64, error) {
	return function.Record{}, 0, errors.New("unused")
}

func (seamFunction) RollbackCreate(context.Context, string, string, string, uint64) error {
	return errors.New("unused")
}

func (seamFunction) BeginCodeUpdate(context.Context, string, string, function.Record, uint64, string) error {
	return errors.New("unused")
}

func (seamFunction) CodeUpdated(context.Context, string, string, function.Record, uint64, string, string, int64) (function.Record, uint64, error) {
	return function.Record{}, 0, errors.New("unused")
}

type urlPresigner struct {
	url   string
	calls *atomic.Int32
}

func (p urlPresigner) PresignGet(context.Context, codestore.Ref, string, time.Duration) (string, error) {
	if p.calls != nil {
		p.calls.Add(1)
	}
	return p.url, nil
}

// countedGrants counts the grants the executor issues.
type countedGrants struct {
	grantIssuer
	issued atomic.Int32
}

func (c *countedGrants) Issue(ctx context.Context, a execution.Attempt) (execution.Attempt, error) {
	c.issued.Add(1)
	return c.grantIssuer.Issue(ctx, a)
}

// skewAhead moves every deadline the agent sees earlier, as a guest clock
// running that far ahead of the host's would.
type skewAhead struct {
	hostlink.Link
	ahead time.Duration
}

func (l skewAhead) Next(ctx context.Context) (hostlink.Request, error) {
	r, err := l.Link.Next(ctx)
	r.DeadlineMs -= l.ahead.Milliseconds()
	return r, err
}

type guestFunc = func(context.Context, net.Conn)

// guestOpts adjusts the loopback guest: a clock running ahead, a sink for the
// first frame's credential, and a gate on reading that frame.
type guestOpts struct {
	ahead time.Duration
	creds chan<- hostlink.Credential
	hold  <-chan struct{}
}

// agentGuest runs the real guest agent and Runtime API server; bootstrap is
// an HTTP client standing in for the function runtime.
func agentGuest(ahead time.Duration, bootstrap func(base string)) guestFunc {
	return agentGuestWith(guestOpts{ahead: ahead}, bootstrap)
}

func agentGuestWith(o guestOpts, bootstrap func(base string)) guestFunc {
	return func(ctx context.Context, conn net.Conn) {
		srv := runtimeapi.New()
		ts := httptest.NewServer(srv.Handler())
		defer ts.Close()
		if bootstrap != nil {
			go bootstrap(ts.URL + "/2018-06-01/runtime/")
		}
		var link hostlink.Link = &credTap{Link: hostlink.NewTestStream(conn), creds: o.creds, hold: o.hold}
		if o.ahead > 0 {
			link = skewAhead{link, o.ahead}
		}
		_ = agent.Run(ctx, link, srv)
	}
}

func nextInvocationID(base string) (string, bool) {
	resp, err := http.Get(base + "invocation/next")
	if err != nil {
		return "", false
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.Header.Get("Lambda-Runtime-Aws-Request-Id"), true
}

// lockedBuffer collects log output written from several goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type seamRun struct {
	env      *seamEnv
	resp     *http.Response
	body     []byte
	requests [][]byte
	replies  [][]byte
	log      string
	presigns int32
	grants   int32
}

// invokeComposed sends one signed Invoke through the composed gateway, the
// Lambda handler and the real test-link executor, over a real control socket,
// into the daemon's attempt, with real IAM and STS issuing the session.
func invokeComposed(t *testing.T, timeout int64, codeURL string, client *http.Client, guest guestFunc) seamRun {
	t.Helper()
	run := invokeComposedAt(t, "/2015-03-31/functions/f/invocations", seamFunction{timeout: timeout}, codeURL, client, guest)
	require.Len(t, run.replies, 1, "the request reached the daemon once")
	return run
}

// invokeComposedAt is invokeComposed for one target and function configuration.
func invokeComposedAt(t *testing.T, target string, fn seamFunction, codeURL string, client *http.Client, guest guestFunc) seamRun {
	t.Helper()
	return invokeComposedIn(t, newSeamEnv(t), target, fn, codeURL, client, guest)
}

// captureLogs sends the default logger to a buffer for the test's duration.
func captureLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	logs := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return logs
}

// composedExecutor is the real test-link executor wired to env's STS and
// owner and to a seam daemon serving guest on a real control socket.
func composedExecutor(t *testing.T, env *seamEnv, codeURL string, client *http.Client, guest guestFunc) (*testLinkExecutor, *seamDaemon, *countedGrants, *atomic.Int32) {
	t.Helper()
	grants := &countedGrants{grantIssuer: env.grants}
	presigns := &atomic.Int32{}
	daemon := startSeamDaemon(t, &testlink.Daemon{Node: "node1", Grants: env.grants, HTTP: client}, guest)
	exec := &testLinkExecutor{Control: daemon.client, RequestLimit: testlink.MaxFrameBytes, Sessions: env.sts, AEAD: env.aead, Node: "node1", ImageDigest: testImageDigest,
		Codes:  func(context.Context) (codePresigner, error) { return urlPresigner{codeURL, presigns}, nil },
		Grants: func(context.Context) (grantIssuer, error) { return grants, nil },
	}
	return exec, daemon, grants, presigns
}

// invokeComposedIn is invokeComposedAt against an existing environment.
func invokeComposedIn(t *testing.T, env *seamEnv, target string, fn seamFunction, codeURL string, client *http.Client, guest guestFunc) seamRun {
	t.Helper()
	logs := captureLogs(t)
	if fn.role == "" {
		fn.role = env.roleARN
	}
	exec, daemon, grants, presigns := composedExecutor(t, env, codeURL, client, guest)
	reg := lambdaawsapi.NewRegistration(lambdaawsapi.Deps{Meta: fn, Exec: exec})
	resp := signedLambdaInvokeAt(t, defaultGateway(t, lambdaTestRoutes(reg)...), target)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	reqs, replies := daemon.recorded()
	return seamRun{env: env, resp: resp, body: body, requests: reqs, replies: replies, log: logs.String(),
		presigns: presigns.Load(), grants: grants.issued.Load()}
}

func codeServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(seamCode)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func daemonReply(t *testing.T, raw []byte) testlink.Reply {
	t.Helper()
	var rep testlink.Reply
	require.NoError(t, json.Unmarshal(raw, &rep))
	return rep
}

// An oversized function response reaches the caller as 200 with an Unhandled
// function error and the runtime's error payload.
func TestLambdaTestLinkSeam_OversizedResponseIsUnhandled(t *testing.T) {
	srv := codeServer(t)
	run := invokeComposed(t, 10, srv.URL+"/code", srv.Client(), agentGuest(0, func(base string) {
		if id, ok := nextInvocationID(base); ok {
			if resp, err := http.Post(base+"invocation/"+id+"/response", "application/json",
				bytes.NewReader(make([]byte, runtimeapi.MaxResponseBytes+1))); err == nil {
				_ = resp.Body.Close()
			}
		}
	}))
	require.Equal(t, testlink.OutcomeDone, daemonReply(t, run.replies[0]).Outcome)
	require.Equal(t, http.StatusOK, run.resp.StatusCode)
	require.Equal(t, "Unhandled", run.resp.Header.Get("X-Amz-Function-Error"))
	var payload struct {
		ErrorType string `json:"errorType"`
	}
	require.NoError(t, json.Unmarshal(run.body, &payload))
	require.Equal(t, "Function.ResponseSizeTooLarge", payload.ErrorType)
}

// Neither an early guest timeout claim nor the host's deadline is a function
// result: both reach the caller as a 500 ServiceException.
func TestLambdaTestLinkSeam_TimeoutsAreUnknown(t *testing.T) {
	cases := map[string]struct {
		timeout int64
		guest   func(context.Context, net.Conn)
		reason  string
	}{
		"agent timeout before the host deadline": {2, agentGuest(2*time.Second-200*time.Millisecond, func(base string) { nextInvocationID(base) }), testlink.ErrTimeoutUnproven.Error()},
		"host deadline":                          {1, func(ctx context.Context, _ net.Conn) { <-ctx.Done() }, testlink.ErrFunctionTimeout.Error()},
		"bootstrap posts the timeout error type": {10, agentGuest(0, postSpoofedTimeout), testlink.ErrTimeoutUnproven.Error()},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := codeServer(t)
			run := invokeComposed(t, tc.timeout, srv.URL+"/code", srv.Client(), tc.guest)
			rep := daemonReply(t, run.replies[0])
			require.Equal(t, testlink.OutcomeUnknown, rep.Outcome)
			require.Equal(t, tc.reason, rep.Error, "the outcome is unknown for the wrong reason")
			require.Equal(t, http.StatusInternalServerError, run.resp.StatusCode)
			require.Equal(t, "ServiceException", run.resp.Header.Get("X-Amzn-Errortype"))
			require.Empty(t, run.resp.Header.Get("X-Amz-Function-Error"))
			require.NotContains(t, string(run.body), "Sandbox.Timedout")
		})
	}
}

// postSpoofedTimeout is a bootstrap reporting Sandbox.Timedout as its own error.
func postSpoofedTimeout(base string) {
	id, ok := nextInvocationID(base)
	if !ok {
		return
	}
	req, err := http.NewRequest(http.MethodPost, base+"invocation/"+id+"/error",
		strings.NewReader(`{"errorType":"Sandbox.Timedout","errorMessage":"spoof"}`))
	if err != nil {
		return
	}
	req.Header.Set("Lambda-Runtime-Function-Error-Type", "Sandbox.Timedout")
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
	}
}

// A malformed redirect quoting a presigned credential reaches neither the
// daemon's reply, the caller, nor the gateway's log.
func TestLambdaTestLinkSeam_RedirectCredentialNotLeaked(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "%zz?X-Amz-Credential="+seamCredential+"&X-Amz-Signature=deadbeef")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	run := invokeComposed(t, 3, srv.URL+"/code", srv.Client(), func(ctx context.Context, _ net.Conn) { <-ctx.Done() })

	rep := daemonReply(t, run.replies[0])
	require.Equal(t, testlink.OutcomeRefused, rep.Outcome)
	require.Equal(t, "code download: request failed", rep.Error)
	require.Contains(t, run.log, "code download: request failed", "the refusal was not logged")
	require.NotContains(t, string(run.replies[0]), seamCredential)
	require.NotContains(t, string(run.body), seamCredential)
	require.NotContains(t, run.log, seamCredential)
	require.Equal(t, http.StatusInternalServerError, run.resp.StatusCode)
}

// The bootstrap sees the ARN the caller invoked, qualifier included, and the
// daemon is asked to run the function's configured handler.
func TestLambdaTestLinkSeam_RuntimeSeesInvokedARNAndHandler(t *testing.T) {
	cases := map[string]struct{ target, suffix string }{
		"unqualified":     {"/2015-03-31/functions/f/invocations", ""},
		"path qualifier":  {"/2015-03-31/functions/f:$LATEST/invocations", ":$LATEST"},
		"query qualifier": {"/2015-03-31/functions/f/invocations?Qualifier=%24LATEST", ":$LATEST"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := codeServer(t)
			seen := make(chan string, 1)
			run := invokeComposedAt(t, tc.target, seamFunction{timeout: 10}, srv.URL+"/code", srv.Client(), agentGuest(0, func(base string) {
				resp, err := http.Get(base + "invocation/next")
				if err != nil {
					seen <- ""
					return
				}
				_ = resp.Body.Close()
				seen <- resp.Header.Get("Lambda-Runtime-Invoked-Function-Arn")
				id := resp.Header.Get("Lambda-Runtime-Aws-Request-Id")
				if post, err := http.Post(base+"invocation/"+id+"/response", "application/json", strings.NewReader(`{}`)); err == nil {
					_ = post.Body.Close()
				}
			}))
			require.Len(t, run.replies, 1)
			require.Equal(t, testlink.OutcomeDone, daemonReply(t, run.replies[0]).Outcome)
			require.Equal(t, http.StatusOK, run.resp.StatusCode)
			var req testlink.Request
			require.NoError(t, json.Unmarshal(run.requests[0], &req))
			require.Equal(t, seamHandler, req.Handler)
			want := arn.FormatLambdaFunction(req.Region, req.AccountID, "f") + tc.suffix
			require.Equal(t, want, <-seen)
		})
	}
}

func TestLambdaTestLinkSeam_EnvironmentReachesGuestWithoutLeaking(t *testing.T) {
	env := newSeamEnv(t)
	var subscriptions []*nats.Subscription
	for _, subject := range []string{">", "$JS.>", "$KV.>"} {
		sub, err := env.nc.SubscribeSync(subject)
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })
		subscriptions = append(subscriptions, sub)
	}
	require.NoError(t, env.nc.Flush())
	vars := map[string]string{"GREETING": "private-environment-marker"}
	sealed, err := function.SealEnv(env.aead, seamAccount, "fn1", vars)
	require.NoError(t, err)
	srv := codeServer(t)
	creds := make(chan hostlink.Credential, 1)
	run := invokeComposedIn(t, env, "/2015-03-31/functions/f/invocations", seamFunction{timeout: 10, envCipher: sealed},
		srv.URL+"/code", srv.Client(), agentGuestWith(guestOpts{creds: creds}, func(base string) { respond(base, `{}`) }))
	require.Equal(t, http.StatusOK, run.resp.StatusCode, string(run.body))
	require.Equal(t, vars, recv(t, creds, "guest credential").Environment)
	for _, request := range run.requests {
		require.NotContains(t, string(request), vars["GREETING"])
	}
	for _, reply := range run.replies {
		require.NotContains(t, string(reply), vars["GREETING"])
	}
	require.NoError(t, env.nc.Flush())
	for _, sub := range subscriptions {
		for {
			msg, err := sub.NextMsg(50 * time.Millisecond)
			if errors.Is(err, nats.ErrTimeout) {
				break
			}
			require.NoError(t, err)
			require.NotContains(t, msg.Subject, vars["GREETING"])
			require.NotContains(t, string(msg.Data), vars["GREETING"])
		}
	}
	require.NotContains(t, run.log, vars["GREETING"])
}
