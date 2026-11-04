//go:build lambdatestlink

package testlink

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/agents/lambda/agent"
	"github.com/mulgadc/spinifex/spinifex/agents/lambda/hostlink"
	"github.com/mulgadc/spinifex/spinifex/agents/lambda/runtimeapi"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/execution"
	"github.com/stretchr/testify/require"
)

const runtimeBase = "/2018-06-01/runtime/"

// countAccept counts how often the owner is asked to accept a result.
func countAccept(n *atomic.Int32) func(context.Context, execution.Grant) error {
	return func(context.Context, execution.Grant) error {
		n.Add(1)
		return nil
	}
}

// skewLink moves every deadline the agent sees earlier by ahead, as a guest
// clock running that far ahead of the host's would.
type skewLink struct {
	hostlink.Link
	ahead time.Duration
}

func (l skewLink) Next(ctx context.Context) (hostlink.Request, error) {
	r, err := l.Link.Next(ctx)
	r.DeadlineMs -= l.ahead.Milliseconds()
	return r, err
}

// runGuest serves the session's guest end with the real agent and Runtime API
// server; bootstrap, if set, is an HTTP client standing in for the function
// runtime, not a launched bootstrap process.
func runGuest(t *testing.T, ctx context.Context, guest net.Conn, ahead time.Duration, bootstrap func(base string)) {
	t.Helper()
	srv := runtimeapi.New()
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	if bootstrap != nil {
		go bootstrap(ts.URL + runtimeBase)
	}
	var link hostlink.Link = hostlink.NewTestStream(guest)
	if ahead > 0 {
		link = skewLink{link, ahead}
	}
	go func() { _ = agent.Run(ctx, link, srv) }()
}

// nextInvocation takes one invocation as a bootstrap does and returns its ID.
func nextInvocation(base string) (string, bool) {
	resp, err := http.Get(base + "invocation/next")
	if err != nil {
		return "", false
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.Header.Get("Lambda-Runtime-Aws-Request-Id"), true
}

func post(base, path string, body []byte, header http.Header) {
	req, err := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return
	}
	for k, v := range header {
		req.Header[k] = v
	}
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
	}
}

// A guest whose clock runs ahead gives up early and claims a timeout before
// the host's deadline; the host does not report that as a result.
func TestSession_GuestClockAheadTimeoutClaimIsUnknown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var accepts atomic.Int32
	guest, done := startSession(t, ctx, testRequest(10_000), countAccept(&accepts))
	// The bootstrap takes the invocation and never answers it.
	runGuest(t, ctx, guest, 10_000*time.Millisecond-200*time.Millisecond, func(base string) { nextInvocation(base) })

	e := <-done
	require.ErrorIs(t, e.err, ErrTimeoutUnproven)
	require.Zero(t, accepts.Load(), "the owner was asked to accept a timeout claim")
	require.Less(t, e.took, 5*time.Second, "the claim arrived before the host's deadline")
	require.Empty(t, e.reply.Payload)
	require.Equal(t, ErrTimeoutUnproven.Error(), safeRunError(e.err))
}

// A bootstrap can post the timeout error type itself; that is not a timeout.
func TestSession_BootstrapPostedTimeoutIsUnknown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var accepts atomic.Int32
	guest, done := startSession(t, ctx, testRequest(10_000), countAccept(&accepts))
	runGuest(t, ctx, guest, 0, func(base string) {
		id, ok := nextInvocation(base)
		if !ok {
			return
		}
		post(base, "invocation/"+id+"/error", []byte(`{"errorType":"Sandbox.Timedout","errorMessage":"spoof"}`),
			http.Header{"Lambda-Runtime-Function-Error-Type": {"Sandbox.Timedout"}})
	})

	e := <-done
	require.ErrorIs(t, e.err, ErrTimeoutUnproven)
	require.Zero(t, accepts.Load(), "the owner was asked to accept a timeout claim")
	require.Empty(t, e.reply.Payload)
}

// An oversized function response comes back as an Unhandled function error
// whose payload names Function.ResponseSizeTooLarge.
func TestSession_OversizedResponseIsUnhandled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	guest, done := startSession(t, ctx, testRequest(10_000), acceptAll)
	runGuest(t, ctx, guest, 0, func(base string) {
		id, ok := nextInvocation(base)
		if !ok {
			return
		}
		post(base, "invocation/"+id+"/response", make([]byte, runtimeapi.MaxResponseBytes+1), nil)
	})

	e := <-done
	require.NoError(t, e.err)
	require.Equal(t, OutcomeDone, e.reply.Outcome)
	require.Equal(t, "Unhandled", e.reply.FunctionError)
	var body struct {
		ErrorType string `json:"errorType"`
	}
	require.NoError(t, json.Unmarshal(e.reply.Payload, &body))
	require.Equal(t, "Function.ResponseSizeTooLarge", body.ErrorType)
}
