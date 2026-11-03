//go:build lambdatestlink

package testlink

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	lambdav1 "github.com/mulgadc/spinifex/contracts/lambda/v1"
	"github.com/mulgadc/spinifex/spinifex/agents/lambda/hostlink"
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
func startSession(t *testing.T, ctx context.Context, req Request) (net.Conn, <-chan sessionEnd) {
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
		r, err := (&Daemon{}).session(ctx, req, gen, ln, workDir)
		done <- sessionEnd{r, err, time.Since(start)}
	}()
	guest, err := hostlink.Dial(ctx, id)
	require.NoError(t, err)
	t.Cleanup(func() { _ = guest.Close() })
	return guest, done
}

func testRequest(timeoutMs int64) Request {
	return Request{AttemptID: "0123456789ab", RequestID: "r", Region: "ap-southeast-2",
		AccountID: "123456789012", FunctionID: "fn1", TimeoutMs: timeoutMs}
}

// A guest that authenticates and then never answers must be cut off at the
// function timeout from dispatch, not at the boot allowance.
func TestSession_UnresponsiveGuestStopsAtTheFunctionTimeout(t *testing.T) {
	// The session's own context allows far longer than the function timeout.
	ctx, cancel := context.WithTimeout(context.Background(), BootAllowance+time.Minute)
	defer cancel()
	_, done := startSession(t, ctx, testRequest(300))

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
	guest, done := startSession(t, ctx, req)

	stream := hostlink.NewTestStream(guest)
	got, err := stream.Next(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Complete(ctx, hostlink.Response{GrantID: got.GrantID,
		AttemptID: got.AttemptID, RequestID: got.RequestID, Payload: []byte(`{"late":true}`)}))

	e := <-done
	require.ErrorIs(t, e.err, ErrFunctionTimeout)
	require.Empty(t, e.reply.Payload)
}
