//go:build lambdatestlink

//test:in-package — exercises the unexported joinAfterFunc and Serve's admission lifecycle
package testlink

import (
	"context"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// A stop after the callback has started waits for it to finish, so no
// close is still running when its caller releases a slot or returns.
func TestJoinAfterFunc_StopWaitsForAStartedCallback(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	started, release := make(chan struct{}), make(chan struct{})
	stop := joinAfterFunc(ctx, func() { close(started); <-release })
	cancel()
	<-started

	returned := make(chan struct{})
	go func() { stop(); close(returned) }()
	select {
	case <-returned:
		t.Fatal("stop returned while the callback was still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("stop never returned after the callback finished")
	}
}

func TestJoinAfterFunc_StopBeforeCancelNeverRunsTheCallback(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	ran := make(chan struct{}, 1)
	stop := joinAfterFunc(ctx, func() { ran <- struct{}{} })
	stop()
	cancel()
	select {
	case <-ran:
		t.Fatal("callback ran after stop")
	case <-time.After(50 * time.Millisecond):
	}
}

// startServe runs Serve with the given IOTimeout and handler and returns a
// cancel that waits for Serve to return, reporting how long that took.
func startServe(t *testing.T, name string, io time.Duration, handle func(context.Context, []byte, Credential) Reply) func() time.Duration {
	t.Helper()
	ln, err := ListenControl(name)
	require.NoError(t, err)
	srv := &ControlServer{GatewayUID: os.Getuid(), RequestLimit: testRequestLimit, ReplyLimit: 4096, IOTimeout: io, Handle: handle}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	var once sync.Once
	var took time.Duration
	stop := func() time.Duration {
		once.Do(func() {
			began := time.Now()
			cancel()
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(20 * time.Second):
				t.Fatal("Serve did not return after cancellation")
			}
			took = time.Since(began)
		})
		return took
	}
	t.Cleanup(func() { stop() })
	return stop
}

// Shutdown closes an admitted peer stalled mid-request and a handler that
// honours cancellation, well inside the 30 s read deadline, leaving no goroutine.
func TestServe_ShutdownClosesAStalledPeerAndACancelledHandler(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	name := controlName(t)
	inHandle := make(chan struct{})
	stop := startServe(t, name, 30*time.Second, func(ctx context.Context, _ []byte, _ Credential) Reply {
		close(inHandle)
		<-ctx.Done()
		return Reply{Outcome: OutcomeUnknown}
	})

	stalled, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: "@" + name, Net: "unix"})
	require.NoError(t, err)
	defer func() { _ = stalled.Close() }()
	// Half a frame header: the server blocks reading the rest.
	_, err = stalled.Write([]byte{0, 0})
	require.NoError(t, err)

	exchanged := make(chan error, 1)
	go func() {
		_, err := client(name, os.Getuid()).Exchange(context.Background(), []byte(`{"attempt_id":"a"}`), testCred, time.Now().Add(30*time.Second))
		exchanged <- err
	}()
	select {
	case <-inHandle:
	case <-time.After(5 * time.Second):
		t.Fatal("second admitted connection never reached Handle")
	}

	took := stop()
	require.Less(t, took, 5*time.Second, "shutdown waited on the read deadline")
	select {
	case <-exchanged:
	case <-time.After(5 * time.Second):
		t.Fatal("gateway exchange never ended after shutdown")
	}
	require.NoError(t, stalled.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err = stalled.Read(make([]byte, 1))
	require.Error(t, err, "the stalled peer's connection was closed")
}

// Cancellation racing normal completion never leaves a close callback or
// connection goroutine behind once Serve returns.
func TestServe_CancellationRacingCompletionLeavesNothingRunning(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	for i := range 50 {
		name := controlName(t)
		stop := startServe(t, name, 5*time.Second, func(context.Context, []byte, Credential) Reply {
			return Reply{Outcome: OutcomeDone, Payload: []byte("ok")}
		})
		// Alternate cancelling before, during and after the exchanges.
		if i%3 == 0 {
			stop()
		}
		var wg sync.WaitGroup
		for range MaxAdmitted {
			wg.Go(func() {
				_, _ = client(name, os.Getuid()).Exchange(context.Background(), []byte(`{"attempt_id":"a"}`), testCred, time.Now().Add(5*time.Second))
			})
		}
		if i%3 == 1 {
			stop()
		}
		wg.Wait()
		stop()
	}
}
