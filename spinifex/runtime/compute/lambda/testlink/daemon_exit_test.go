//go:build lambdatestlink

package testlink

import (
	"context"
	"errors"
	"net"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/agents/lambda/agent"
	"github.com/mulgadc/spinifex/spinifex/agents/lambda/hostlink"
	"github.com/mulgadc/spinifex/spinifex/agents/lambda/runtimeapi"
	"github.com/stretchr/testify/require"
)

// runExitingGuest serves the guest end with the real agent and Runtime API
// and applies a bootstrap exit as the guest binary does: only an exit before
// a response is admitted cancels the invocation. The guest connection closes
// when the agent stops, as the process exit would.
func runExitingGuest(t *testing.T, ctx context.Context, guest net.Conn, bootstrap func(base string, exit func())) {
	t.Helper()
	srv := runtimeapi.New()
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	actx, cancel := context.WithCancel(ctx)
	exit := func() {
		if srv.BootstrapExited() {
			cancel()
		}
	}
	go bootstrap(ts.URL+runtimeBase, exit)
	go func() {
		defer cancel()
		defer guest.Close()
		_ = agent.Run(actx, hostlink.NewTestStream(guest), srv)
	}()
	_ = ts
}

// partialPost sends a response whose body ends before its declared length.
func partialPost(t *testing.T, addr, id string) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return
	}
	_, _ = conn.Write([]byte("POST " + runtimeBase + "invocation/" + id + "/response HTTP/1.1\r\nHost: x\r\nContent-Length: 1000\r\n\r\npartial"))
	_ = conn.Close()
}

func TestSession_ResponseThenImmediateExitStillAccepted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var accepts atomic.Int32
	guest, done := startSession(t, ctx, testRequest(20_000), countAccept(&accepts))
	payload := make([]byte, runtimeapi.MaxResponseBytes)
	for i := range payload {
		payload[i] = byte(i)
	}
	runExitingGuest(t, ctx, guest, func(base string, exit func()) {
		id, ok := nextInvocation(base)
		if !ok {
			return
		}
		post(base, "invocation/"+id+"/response", payload, nil)
		exit()
	})

	e := <-done
	require.NoError(t, e.err)
	require.Equal(t, OutcomeDone, e.reply.Outcome)
	require.Equal(t, payload, e.reply.Payload)
	require.Equal(t, int32(1), accepts.Load())
}

func TestSession_ExitBeforeAResponseIsUnknown(t *testing.T) {
	cases := map[string]func(base string, exit func()){
		"before next": func(_ string, exit func()) { exit() },
		"after next": func(base string, exit func()) {
			nextInvocation(base)
			exit()
		},
		"mid body with the bootstrap alive": func(base string, _ func()) {
			id, ok := nextInvocation(base)
			if !ok {
				return
			}
			partialPost(t, base[len("http://"):len(base)-len(runtimeBase)], id)
		},
		"mid body then exit": func(base string, exit func()) {
			id, ok := nextInvocation(base)
			if !ok {
				return
			}
			partialPost(t, base[len("http://"):len(base)-len(runtimeBase)], id)
			exit()
		},
	}
	for name, bootstrap := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var accepts atomic.Int32
			guest, done := startSession(t, ctx, testRequest(20_000), countAccept(&accepts))
			runExitingGuest(t, ctx, guest, bootstrap)

			select {
			case e := <-done:
				require.Error(t, e.err)
				require.False(t, errors.Is(e.err, ErrResultNotAccepted))
				require.Empty(t, e.reply.Payload)
				require.Zero(t, accepts.Load(), "the owner was asked to accept a lost response")
				require.Less(t, e.took, 15*time.Second, "the host waited for its deadline")
			case <-time.After(25 * time.Second):
				t.Fatal("the host did not report")
			}
		})
	}
}

func TestSession_AllowedResponseAtCapIsDone(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var accepts atomic.Int32
	guest, done := startSession(t, ctx, testRequest(20_000), countAccept(&accepts))
	runGuest(t, ctx, guest, 0, func(base string) {
		id, ok := nextInvocation(base)
		if !ok {
			return
		}
		post(base, "invocation/"+id+"/response", make([]byte, runtimeapi.MaxResponseBytes), nil)
	})
	e := <-done
	require.NoError(t, e.err)
	require.Len(t, e.reply.Payload, runtimeapi.MaxResponseBytes)
	require.Equal(t, int32(1), accepts.Load())
}
