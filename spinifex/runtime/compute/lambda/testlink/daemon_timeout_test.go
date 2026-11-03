//go:build lambdatestlink

package testlink

import (
	"context"
	"errors"
	"math"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	lambdav1 "github.com/mulgadc/spinifex/contracts/lambda/v1"
	"github.com/mulgadc/spinifex/spinifex/agents/lambda/hostlink"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/execution"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/lambda/guestconn"
	"github.com/stretchr/testify/require"
)

type sessionEnd struct {
	reply Reply
	err   error
	took  time.Duration
}

// startSession runs one session against a real authenticated guest
// connection and returns that connection and the session's end.
func startSession(t *testing.T, ctx context.Context, req Request, accept func(context.Context, execution.Grant) error) (net.Conn, <-chan sessionEnd) {
	t.Helper()
	gen, err := guestconn.NewGeneration(lambdav1.Scope(scope(req)), time.Hour)
	require.NoError(t, err)
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	guestDir := t.TempDir()
	for name, b := range gen.GuestEntries(ln.Addr().String()) {
		require.NoError(t, os.MkdirAll(filepath.Join(guestDir, name), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(guestDir, name, "raw"), b, 0o600))
	}
	id, err := hostlink.LoadIdentity(guestDir)
	require.NoError(t, err)

	done := make(chan sessionEnd, 1)
	workDir := t.TempDir()
	go func() {
		start := time.Now()
		r, err := (&Daemon{}).session(ctx, req, gen, ln, workDir, sessionDeadline, accept)
		done <- sessionEnd{r, err, time.Since(start)}
	}()
	guest, err := hostlink.Dial(ctx, id)
	require.NoError(t, err)
	t.Cleanup(func() { _ = guest.Close() })
	return guest, done
}

func testRequest(timeoutMs int64) Request {
	return Request{Grant: execution.Grant{GrantID: "g1", AttemptID: "0123456789ab", EnvironmentID: "env-1", Generation: 1},
		AttemptID: "0123456789ab", RequestID: "r", Region: "ap-southeast-2",
		AccountID: "123456789012", FunctionID: "fn1", TimeoutMs: timeoutMs}
}

func acceptAll(context.Context, execution.Grant) error { return nil }

// A guest that authenticates and then never answers must be cut off at the
// function timeout from dispatch, not at the boot allowance.
func TestSession_UnresponsiveGuestStopsAtTheFunctionTimeout(t *testing.T) {
	// The session's own context allows far longer than the function timeout.
	ctx, cancel := context.WithTimeout(context.Background(), BootAllowance+time.Minute)
	defer cancel()
	_, done := startSession(t, ctx, testRequest(300), acceptAll)

	select {
	case e := <-done:
		require.True(t, errors.Is(e.err, ErrFunctionTimeout), "got %v", e.err)
		require.Less(t, e.took, BootAllowance/3)
		require.Equal(t, ErrFunctionTimeout.Error(), safeRunError(e.err))
	case <-time.After(BootAllowance):
		t.Fatal("session waited past the function timeout for an unresponsive guest")
	}
}

// A valid result read once the deadline has passed is a timeout, not a result.
func TestSession_ResultAfterTheDeadlineIsATimeout(t *testing.T) {
	// The first reading sets the deadline; every later one is an hour on.
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
	req := testRequest(10_000)
	guest, done := startSession(t, ctx, req, acceptAll)

	stream := hostlink.NewTestStream(guest)
	got, err := stream.Next(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Complete(ctx, hostlink.Response{GrantID: got.GrantID,
		AttemptID: got.AttemptID, RequestID: got.RequestID, Payload: []byte(`{"late":true}`)}))

	e := <-done
	require.ErrorIs(t, e.err, ErrFunctionTimeout)
	require.Empty(t, e.reply.Payload)
}

// A result the owner will not accept under the grant is never reported, and
// the guest sees the grant ID the owner issued.
func TestSession_ResultTheOwnerRefusesIsNotReported(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	refuse := func(context.Context, execution.Grant) error { return execution.ErrNotCurrent }
	guest, done := startSession(t, ctx, testRequest(10_000), refuse)

	stream := hostlink.NewTestStream(guest)
	got, err := stream.Next(ctx)
	require.NoError(t, err)
	require.Equal(t, "g1", got.GrantID)
	require.NoError(t, stream.Complete(ctx, hostlink.Response{GrantID: got.GrantID,
		AttemptID: got.AttemptID, RequestID: got.RequestID, Payload: []byte(`{"ok":true}`)}))

	e := <-done
	require.ErrorIs(t, e.err, ErrResultNotAccepted)
	require.ErrorIs(t, e.err, execution.ErrNotCurrent)
	require.Empty(t, e.reply.Payload)
	require.Equal(t, ErrResultNotAccepted.Error(), safeRunError(e.err))
}

// An acceptance the owner records only after the deadline is a timeout, and
// Accept runs under the dispatch deadline.
func TestSession_AcceptanceAfterTheDeadlineIsATimeout(t *testing.T) {
	start := time.Now()
	var late atomic.Bool
	now = func() time.Time {
		if late.Load() {
			return start.Add(time.Hour)
		}
		return start
	}
	t.Cleanup(func() { now = time.Now })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var acceptDeadline time.Time
	slowAccept := func(actx context.Context, _ execution.Grant) error {
		acceptDeadline, _ = actx.Deadline()
		late.Store(true)
		return nil
	}
	guest, done := startSession(t, ctx, testRequest(10_000), slowAccept)

	stream := hostlink.NewTestStream(guest)
	got, err := stream.Next(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Complete(ctx, hostlink.Response{GrantID: got.GrantID,
		AttemptID: got.AttemptID, RequestID: got.RequestID, Payload: []byte(`{"ok":true}`)}))

	e := <-done
	require.ErrorIs(t, e.err, ErrFunctionTimeout)
	require.Empty(t, e.reply.Payload)
	require.Equal(t, start.Add(10*time.Second), acceptDeadline, "Accept is bounded by the dispatch deadline")
}

// sessionDeadline is the fixed boottime deadline startSession passes; far
// ahead unless a test moves it.
var sessionDeadline = time.Duration(math.MaxInt64)

// A session whose guest accept outlasts the fixed deadline sends no frame.
func TestSession_ExpiredBeforeHostSendSendsNothing(t *testing.T) {
	sessionDeadline = 0
	t.Cleanup(func() { sessionDeadline = time.Duration(math.MaxInt64) })
	req := testRequest(1000)
	called := false
	guest, done := startSession(t, context.Background(), req, func(context.Context, execution.Grant) error {
		called = true
		return nil
	})
	end := <-done
	require.ErrorIs(t, end.err, ErrDeadlineExpired)
	require.False(t, called)
	_ = guest.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _ := guest.Read(make([]byte, 1))
	require.Zero(t, n, "no frame reached the guest")
}
