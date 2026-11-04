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
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/agents/lambda/agent"
	"github.com/mulgadc/spinifex/spinifex/agents/lambda/hostlink"
	"github.com/mulgadc/spinifex/spinifex/agents/lambda/runtimeapi"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/execution"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
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
	ts       *httptest.Server
	base     string // host:port of the Runtime API
	exit     func()
	admitted chan struct{} // closed when a POST's body is first read
	runErr   chan error    // agent.Run's return
	bdone    chan struct{} // closed when the bootstrap function returns
	drained  chan struct{} // closed when the guest stream's reader has finished
}

// startExitingGuest runs bootstrap against the guest. Errors a bootstrap
// reports through t fail the test; the caller waits on g.bdone.
func startExitingGuest(t *testing.T, ctx context.Context, guest net.Conn, gate chan struct{},
	bootstrap func(g *exitingGuest)) *exitingGuest {
	t.Helper()
	srv := runtimeapi.New()
	g := &exitingGuest{admitted: make(chan struct{}), runErr: make(chan error, 1), bdone: make(chan struct{}), drained: make(chan struct{})}
	var once sync.Once
	h := srv.Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			r.Body = firstRead{r.Body, &once, g.admitted}
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	g.ts = ts
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
		defer close(g.drained)
		stream := hostlink.NewTestStream(&gateConn{Conn: guest, gate: gate})
		g.runErr <- agent.Run(actx, stream, srv)
		// The connection closes when the agent stops, as its process exit
		// would; the stream is then read to its end so its reader returns.
		_ = guest.Close()
		for {
			// A shutdown frame is a consumed frame; only the terminal read
			// error means the reader has exited.
			if _, err := stream.Next(context.Background()); err != nil && !errors.Is(err, hostlink.ErrShutdown) {
				return
			}
		}
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
	// Closed on every exit path, or the server's Close would wait on the handler.
	t.Cleanup(func() { _ = conn.Close() })
	req := "POST " + runtimeBase + "invocation/" + id + "/response HTTP/1.1\r\nHost: x\r\nContent-Length: 1000\r\n\r\npartial"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Errorf("bootstrap partial post: %v", err)
	}
	return conn
}

// gateRelease returns a function that opens gate exactly once, registered to
// run at cleanup so a failing test never strands a blocked write.
func gateRelease(t *testing.T, gate chan struct{}) func() {
	t.Helper()
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	return release
}

// recv bounds a wait so a regression fails promptly instead of hanging.
func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(15 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
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
	release := gateRelease(t, gate)
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
		release()
	}

	e := recv(t, done, "the host's outcome")
	recv(t, g.bdone, "the bootstrap to return")
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

			e := recv(t, done, "the host's outcome")
			recv(t, g.bdone, "the bootstrap to return")
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

			e := recv(t, done, "the host's outcome")
			recv(t, g.bdone, "the bootstrap to return")
			require.ErrorIs(t, recv(t, g.runErr, "agent.Run to return"), runtimeapi.ErrResponseBodyFailed)
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
	e := recv(t, done, "the host's outcome")
	require.NoError(t, e.err)
	require.Len(t, e.reply.Payload, runtimeapi.MaxResponseBytes)
	require.Equal(t, int32(1), accepts.Load())
}

// rawGuest reads the host's request over the authenticated connection and
// returns what a hand-written result must echo.
func rawGuest(t *testing.T, ctx context.Context, guest net.Conn) (*hostlink.TestStream, hostlink.Response) {
	t.Helper()
	stream := hostlink.NewTestStream(guest)
	req, err := stream.Next(ctx)
	require.NoError(t, err)
	return stream, hostlink.Response{GrantID: req.GrantID, AttemptID: req.AttemptID, RequestID: req.RequestID, Payload: []byte(`{"ok":true}`)}
}

func TestSession_IncompleteResultFrameThenCloseIsUnknown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var accepts atomic.Int32
	guest, done := startSession(t, ctx, testRequest(20_000), countAccept(&accepts))
	_, _ = rawGuest(t, ctx, guest)
	// A length prefix promising 1000 bytes, then 40 of them, then the end.
	partial := append([]byte{0, 0, 3, 232}, []byte(`{"type":"result","response":{"grant_id":"g1"`)...)
	_, err := guest.Write(partial)
	require.NoError(t, err)
	require.NoError(t, guest.Close())

	e := recv(t, done, "the host's outcome")
	require.Error(t, e.err)
	require.Empty(t, e.reply.Payload)
	require.Zero(t, accepts.Load(), "a partial frame must not be accepted")
}

func TestSession_FullFrameReadBeforeCloseFollowsTheHostsOwnChecks(t *testing.T) {
	cases := map[string]struct {
		acceptErr error
		wantErr   error
		accepts   int32
	}{
		"accepted":         {nil, nil, 1},
		"grant not current": {execution.ErrNotCurrent, ErrResultNotAccepted, 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var accepts atomic.Int32
			hostRead := make(chan struct{})
			var once sync.Once
			accept := func(context.Context, execution.Grant) error {
				accepts.Add(1)
				once.Do(func() { close(hostRead) })
				return tc.acceptErr
			}
			guest, done := startSession(t, ctx, testRequest(20_000), accept)
			stream, resp := rawGuest(t, ctx, guest)
			require.NoError(t, stream.Complete(ctx, resp))
			// Close only once the host holds the whole frame.
			recv(t, hostRead, "the host to read the frame")
			require.NoError(t, guest.Close())

			e := recv(t, done, "the host's outcome")
			if tc.wantErr == nil {
				require.NoError(t, e.err)
				require.Equal(t, OutcomeDone, e.reply.Outcome)
				require.Equal(t, resp.Payload, e.reply.Payload)
			} else {
				require.ErrorIs(t, e.err, tc.wantErr)
				require.Empty(t, e.reply.Payload)
			}
			require.Equal(t, tc.accepts, accepts.Load())
		})
	}
}

func TestSession_LateResultNeverReachesAccept(t *testing.T) {
	var reads atomic.Int32
	start := time.Now()
	now = func() time.Time {
		if reads.Add(1) == 1 {
			return start
		}
		return start.Add(time.Hour)
	}
	t.Cleanup(func() { now = time.Now })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var accepts atomic.Int32
	guest, done := startSession(t, ctx, testRequest(10_000), countAccept(&accepts))
	stream, resp := rawGuest(t, ctx, guest)
	require.NoError(t, stream.Complete(ctx, resp))

	e := recv(t, done, "the host's outcome")
	require.ErrorIs(t, e.err, ErrFunctionTimeout)
	require.Zero(t, accepts.Load())
}

// A body held open past the invocation deadline ends as unknown. The ignore
// covers every hostlink readLoop and exists for the host session's own reader,
// whose shutdown is tracked separately; the guest reader is shown finished
// by the drain channel.
func TestSession_SlowAdmittedBodyHeldPastTheDeadlineIsUnknown(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent(),
		goleak.IgnoreAnyFunction("github.com/mulgadc/spinifex/spinifex/agents/lambda/hostlink.(*TestStream).readLoop"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var accepts atomic.Int32
	guest, done := startSession(t, ctx, testRequest(3000), countAccept(&accepts))
	held := make(chan net.Conn, 1)
	g := startExitingGuest(t, ctx, guest, nil, func(g *exitingGuest) {
		id, ok := g.next(t)
		if !ok {
			return
		}
		if conn := g.startPartial(t, id); conn != nil {
			held <- conn
		}
	})

	// Admission must come before the host's deadline, not after it.
	var e sessionEnd
	select {
	case <-g.admitted:
		// A ready select picks at random, so confirm the host is still waiting.
		select {
		case e = <-done:
			t.Fatalf("the host finished before the POST was admitted: %v", e.err)
		default:
		}
	case e = <-done:
		t.Fatalf("the host finished before the POST was admitted: %v", e.err)
	case <-time.After(10 * time.Second):
		t.Fatal("the POST was not admitted")
	}
	e = recv(t, done, "the host's outcome")
	require.Error(t, e.err)
	require.Empty(t, e.reply.Payload)
	require.Zero(t, accepts.Load())

	// The agent ends with the timeout or a link error, whichever lands first.
	require.Error(t, recv(t, g.runErr, "agent.Run to return"), "a clean return cannot pass")
	recv(t, g.bdone, "the bootstrap to return")
	recv(t, g.drained, "the guest stream's reader to finish")
	_ = recv(t, held, "the held connection").Close()
	closed := make(chan struct{})
	go func() { g.ts.Close(); close(closed) }()
	recv(t, closed, "the HTTP handler to finish")
	_ = guest.Close()
	http.DefaultTransport.(*http.Transport).CloseIdleConnections()
	// Observation only: which readLoop goroutines remain, for the report.
	dump := make([]byte, 1<<20)
	dump = dump[:runtime.Stack(dump, true)]
	t.Logf("readLoop goroutines still running: %d", strings.Count(string(dump), "TestStream).readLoop"))
}
