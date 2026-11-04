//go:build lambdatestlink

package testlink

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/agents/lambda/agent"
	"github.com/mulgadc/spinifex/spinifex/agents/lambda/hostlink"
	"github.com/mulgadc/spinifex/spinifex/agents/lambda/runtimeapi"
	"github.com/stretchr/testify/require"
)

// gateConn holds back the guest's writes until gate is closed, standing in
// for a host that has not yet read.
type gateConn struct {
	net.Conn
	gate chan struct{}
}

func (c *gateConn) Write(p []byte) (int, error) {
	if c.gate != nil {
		<-c.gate
	}
	return c.Conn.Write(p)
}

// firstRead closes ch the first time the handler reads the request body,
// which it does only after admitting the POST.
type firstRead struct {
	io.ReadCloser
	once *sync.Once
	ch   chan struct{}
}

func (b firstRead) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.ch) })
	return b.ReadCloser.Read(p)
}

// exitingGuest is the guest end: the real agent and Runtime API, with a
// bootstrap exit applied as the guest binary does, so only an exit before a
// response is admitted cancels the invocation.
type exitingGuest struct {
	base     string // host:port of the Runtime API
	exit     func()
	admitted chan struct{} // closed when a POST's body is first read
	runErr   chan error    // agent.Run's return
	bdone    chan struct{} // closed when the bootstrap function returns
}

// startExitingGuest runs bootstrap against the guest. Errors a bootstrap
// reports through t fail the test; the caller waits on g.bdone.
func startExitingGuest(t *testing.T, ctx context.Context, guest net.Conn, gate chan struct{},
	bootstrap func(g *exitingGuest)) *exitingGuest {
	t.Helper()
	srv := runtimeapi.New()
	g := &exitingGuest{admitted: make(chan struct{}), runErr: make(chan error, 1), bdone: make(chan struct{})}
	var once sync.Once
	h := srv.Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			r.Body = firstRead{r.Body, &once, g.admitted}
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	g.base = ts.Listener.Addr().String()
	actx, cancel := context.WithCancel(ctx)
	g.exit = func() {
		if srv.BootstrapExited() {
			cancel()
		}
	}
	go func() {
		defer close(g.bdone)
		bootstrap(g)
	}()
	go func() {
		defer cancel()
		// The connection closes when the agent stops, as its process exit would.
		defer guest.Close()
		g.runErr <- agent.Run(actx, hostlink.NewTestStream(&gateConn{Conn: guest, gate: gate}), srv)
	}()
	return g
}

func (g *exitingGuest) url() string { return "http://" + g.base + runtimeBase }

// next takes the invocation, failing the test on any transport error.
func (g *exitingGuest) next(t *testing.T) (string, bool) {
	t.Helper()
	resp, err := http.Get(g.url() + "invocation/next")
	if err != nil {
		t.Errorf("bootstrap next: %v", err)
		return "", false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Header.Get("Lambda-Runtime-Aws-Request-Id"), true
}

// respond posts a whole response and requires it to be accepted.
func (g *exitingGuest) respond(t *testing.T, id string, body []byte) {
	t.Helper()
	resp, err := http.Post(g.url()+"invocation/"+id+"/response", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Errorf("bootstrap response: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("bootstrap response: status %d", resp.StatusCode)
	}
}

// startPartial sends a response whose body stops short of its declared length
// and returns the open connection.
func (g *exitingGuest) startPartial(t *testing.T, id string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", g.base)
	if err != nil {
		t.Errorf("bootstrap dial: %v", err)
		return nil
	}
	req := "POST " + runtimeBase + "invocation/" + id + "/response HTTP/1.1\r\nHost: x\r\nContent-Length: 1000\r\n\r\npartial"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Errorf("bootstrap partial post: %v", err)
	}
	return conn
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) bool {
	t.Helper()
	select {
	case <-ch:
		return true
	case <-time.After(10 * time.Second):
		t.Errorf("timed out waiting for %s", what)
		return false
	}
}

func TestSession_ResponseThenExitWhileHostHasNotReadStillAccepted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var accepts atomic.Int32
	guest, done := startSession(t, ctx, testRequest(20_000), countAccept(&accepts))
	payload := make([]byte, runtimeapi.MaxResponseBytes)
	for i := range payload {
		payload[i] = byte(i)
	}
	gate := make(chan struct{})
	exited := make(chan struct{})
	g := startExitingGuest(t, ctx, guest, gate, func(g *exitingGuest) {
		id, ok := g.next(t)
		if !ok {
			return
		}
		g.respond(t, id, payload)
		g.exit()
		close(exited)
	})
	// The exit has been applied before the host sees a byte of the result.
	if waitFor(t, exited, "the bootstrap to exit") {
		close(gate)
	}

	e := <-done
	<-g.bdone
	require.NoError(t, e.err)
	require.Equal(t, OutcomeDone, e.reply.Outcome)
	require.Equal(t, payload, e.reply.Payload)
	require.Equal(t, int32(1), accepts.Load())
}

func TestSession_LostResponseIsUnknown(t *testing.T) {
	cases := map[string]func(g *exitingGuest){
		"exit before next": func(g *exitingGuest) { g.exit() },
		"exit after next": func(g *exitingGuest) {
			g.next(t)
			g.exit()
		},
	}
	for name, bootstrap := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var accepts atomic.Int32
			guest, done := startSession(t, ctx, testRequest(20_000), countAccept(&accepts))
			g := startExitingGuest(t, ctx, guest, nil, bootstrap)

			e := <-done
			<-g.bdone
			require.Error(t, e.err)
			require.False(t, errors.Is(e.err, ErrResultNotAccepted))
			require.Empty(t, e.reply.Payload)
			require.Zero(t, accepts.Load(), "the owner was asked to accept a lost response")
			require.Less(t, e.took, 15*time.Second, "the host waited for its deadline")
		})
	}
}

// The POST is proved admitted before its body fails, with the bootstrap alive
// and then exiting; the agent must end with the body error and send nothing.
func TestSession_AdmittedBodyLostIsUnknown(t *testing.T) {
	for _, exitAfter := range []bool{false, true} {
		name := "bootstrap alive"
		if exitAfter {
			name = "bootstrap exits after"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var accepts atomic.Int32
			guest, done := startSession(t, ctx, testRequest(20_000), countAccept(&accepts))
			g := startExitingGuest(t, ctx, guest, nil, func(g *exitingGuest) {
				id, ok := g.next(t)
				if !ok {
					return
				}
				conn := g.startPartial(t, id)
				if conn == nil {
					return
				}
				if waitFor(t, g.admitted, "the server to admit the POST") {
					conn.Close() // the body now fails
				}
				if exitAfter {
					g.exit()
				}
			})

			e := <-done
			<-g.bdone
			require.ErrorIs(t, <-g.runErr, runtimeapi.ErrResponseBodyFailed)
			require.Error(t, e.err)
			require.Empty(t, e.reply.Payload)
			require.Zero(t, accepts.Load(), "the owner was asked to accept a lost response")
			require.Less(t, e.took, 15*time.Second, "the host waited for its deadline")
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
