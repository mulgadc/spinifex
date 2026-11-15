//go:build e2e

package harness

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A suite already marked broken must skip the caller rather than answer false
// at once: callers read false as "waited the whole budget and the guest never
// answered", and report a timeout for a wait that never happened.
func TestTryReadySkipsOnceTheSuiteIsBroken(t *testing.T) {
	var h SSHHealth
	h.broken.Store(true)

	var probe *testing.T
	returned := false
	t.Run("probe", func(t *testing.T) {
		probe = t
		h.TryReady(t, "192.0.2.1", 22, "/nonexistent", time.Millisecond)
		returned = true
	})

	if !probe.Skipped() {
		t.Fatal("TryReady on a broken suite did not skip its caller")
	}
	if returned {
		t.Fatal("TryReady on a broken suite returned to its caller instead of skipping")
	}
}

func TestWaitReadyDecision(t *testing.T) {
	cases := map[string]struct {
		attempts   []bool
		withOnMiss bool
		wantReady  bool
		wantCalls  []string
	}{
		"a first-attempt success neither dumps nor re-primes": {
			attempts: []bool{true}, withOnMiss: true, wantReady: true,
			wantCalls: []string{"attempt"},
		},
		"a miss dumps before the re-prime and the retry recovers": {
			attempts: []bool{false, true}, withOnMiss: true, wantReady: true,
			wantCalls: []string{"attempt", "onMiss", "reprime", "attempt"},
		},
		"two misses dump once and fail": {
			attempts: []bool{false, false}, withOnMiss: true, wantReady: false,
			wantCalls: []string{"attempt", "onMiss", "reprime", "attempt"},
		},
		"a nil onMiss still re-primes and fails": {
			attempts: []bool{false, false}, wantReady: false,
			wantCalls: []string{"attempt", "reprime", "attempt"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var calls []string
			n := 0
			attempt := func(time.Duration) bool {
				calls = append(calls, "attempt")
				n++
				return tc.attempts[n-1]
			}
			var onMiss func()
			if tc.withOnMiss {
				onMiss = func() { calls = append(calls, "onMiss") }
			}
			got := waitReadyDecision(attempt, func() { calls = append(calls, "reprime") }, onMiss)
			if got != tc.wantReady {
				t.Fatalf("ready = %v, want %v", got, tc.wantReady)
			}
			if strings.Join(calls, ",") != strings.Join(tc.wantCalls, ",") {
				t.Fatalf("calls = %v, want %v", calls, tc.wantCalls)
			}
		})
	}
}

// A broken suite skips before any wait, so no diagnostic is dumped for a
// guest that was never probed.
func TestWaitReadyOrSkipsWithoutDumpingOnceTheSuiteIsBroken(t *testing.T) {
	var h SSHHealth
	h.broken.Store(true)

	var probe *testing.T
	dumped := false
	t.Run("probe", func(t *testing.T) {
		probe = t
		h.WaitReadyOr(t, "192.0.2.1", 22, "/nonexistent", func() { dumped = true })
	})

	if !probe.Skipped() {
		t.Fatal("WaitReadyOr on a broken suite did not skip its caller")
	}
	if dumped {
		t.Fatal("WaitReadyOr on a broken suite called onMiss")
	}
}

// A handshake that stalls after connecting is cancelled at the budget, so the
// first miss, and the diagnostic dump behind it, is not held back by it.
func TestPollHandshakeCancelsAStalledAttemptAtTheBudget(t *testing.T) {
	start := time.Now()
	attempts := 0
	got := pollHandshake(100*time.Millisecond, 50*time.Millisecond, time.Millisecond, func(ctx context.Context) bool {
		attempts++
		<-ctx.Done()
		return false
	})
	if got {
		t.Fatal("a stalled handshake reported success")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("a stalled handshake held the wait for %s against a 100ms budget", elapsed)
	}
	if attempts == 0 {
		t.Fatal("no attempt was made")
	}
}

func TestPollHandshakeReturnsOnTheFirstSuccess(t *testing.T) {
	attempts := 0
	got := pollHandshake(time.Second, 50*time.Millisecond, time.Millisecond, func(context.Context) bool {
		attempts++
		return attempts == 2
	})
	if !got || attempts != 2 {
		t.Fatalf("ready = %v after %d attempts, want true after 2", got, attempts)
	}
}
