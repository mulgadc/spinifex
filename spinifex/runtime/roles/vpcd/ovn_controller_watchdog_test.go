package vpcd

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeSBProber scripts SBConnectionState and counts resets.
type fakeSBProber struct {
	status    string
	statusErr error
	resetErr  error
	resets    int
	calls     int
}

func (f *fakeSBProber) SBConnectionState(_ context.Context) (string, error) {
	f.calls++
	return f.status, f.statusErr
}

func (f *fakeSBProber) ResetSBClusterState(_ context.Context) error {
	f.resets++
	return f.resetErr
}

func newTestWatchdog(f *fakeSBProber) *ovnWatchdog {
	return &ovnWatchdog{prober: f, staleAfter: 10 * time.Second, cooldown: 60 * time.Second}
}

func TestOVNWatchdog_ConnectedNeverResets(t *testing.T) {
	f := &fakeSBProber{status: "connected"}
	w := newTestWatchdog(f)
	base := time.Unix(1_000_000, 0)
	for i := range 5 {
		if w.evaluate(context.Background(), base.Add(time.Duration(i)*time.Minute)) {
			t.Fatalf("evaluate reset while connected (tick %d)", i)
		}
	}
	if f.resets != 0 {
		t.Errorf("resets = %d, want 0 (connected status must never reset)", f.resets)
	}
}

func TestOVNWatchdog_StaleUnderThresholdNoReset(t *testing.T) {
	f := &fakeSBProber{status: "not connected"}
	w := newTestWatchdog(f)
	base := time.Unix(1_000_000, 0)
	w.evaluate(context.Background(), base)                    // arm at t0
	w.evaluate(context.Background(), base.Add(9*time.Second)) // still under staleAfter (10s)
	if f.resets != 0 {
		t.Errorf("resets = %d, want 0 (a sub-threshold blip must not reset)", f.resets)
	}
}

func TestOVNWatchdog_StalePastThresholdResetsOnce(t *testing.T) {
	f := &fakeSBProber{status: "not connected"}
	w := newTestWatchdog(f)
	base := time.Unix(1_000_000, 0)
	w.evaluate(context.Background(), base) // arm at t0
	if !w.evaluate(context.Background(), base.Add(11*time.Second)) {
		t.Fatal("evaluate did not reset past the stale threshold")
	}
	if f.resets != 1 {
		t.Errorf("resets = %d, want 1", f.resets)
	}
}

func TestOVNWatchdog_CooldownBlocksTightLoop(t *testing.T) {
	f := &fakeSBProber{status: "not connected"}
	w := newTestWatchdog(f)
	base := time.Unix(1_000_000, 0)
	w.evaluate(context.Background(), base)                     // arm
	w.evaluate(context.Background(), base.Add(11*time.Second)) // reset #1 (lastReset=+11s)
	// Re-arm and cross the stale threshold again, but stay within the 60s cooldown:
	w.evaluate(context.Background(), base.Add(12*time.Second)) // re-arm at +12s
	w.evaluate(context.Background(), base.Add(30*time.Second)) // stale (18s) but cooldown (19s<60s)
	if f.resets != 1 {
		t.Errorf("resets = %d, want 1 (cooldown must block a second reset)", f.resets)
	}
	// Past the cooldown, a still-wedged SB resets again.
	if !w.evaluate(context.Background(), base.Add(80*time.Second)) {
		t.Fatal("evaluate did not reset after the cooldown elapsed")
	}
	if f.resets != 2 {
		t.Errorf("resets = %d, want 2 (reset resumes after cooldown)", f.resets)
	}
}

func TestOVNWatchdog_RecoveryClearsStaleTimer(t *testing.T) {
	f := &fakeSBProber{status: "not connected"}
	w := newTestWatchdog(f)
	base := time.Unix(1_000_000, 0)
	w.evaluate(context.Background(), base) // arm at t0
	f.status = "connected"
	w.evaluate(context.Background(), base.Add(5*time.Second)) // recovered; stale timer cleared
	f.status = "not connected"
	w.evaluate(context.Background(), base.Add(6*time.Second))  // re-arm at +6s
	w.evaluate(context.Background(), base.Add(12*time.Second)) // only 6s stale (< 10s) -> no reset
	if f.resets != 0 {
		t.Errorf("resets = %d, want 0 (a recovery must reset the stale clock)", f.resets)
	}
}

func TestOVNWatchdog_ProbeErrorNoReset(t *testing.T) {
	f := &fakeSBProber{statusErr: errors.New("appctl unavailable")}
	w := newTestWatchdog(f)
	base := time.Unix(1_000_000, 0)
	w.evaluate(context.Background(), base)
	w.evaluate(context.Background(), base.Add(time.Hour))
	if f.resets != 0 {
		t.Errorf("resets = %d, want 0 (a probe error is unknown, not a wedge)", f.resets)
	}
}

// TestOVNWatchdog_ResetErrorRecordsAttempt covers the reset-failure branch: even
// when sb-cluster-state-reset errors, evaluate reports the attempt and records it so
// the cooldown throttles retries rather than hammering a genuinely-down SB.
func TestOVNWatchdog_ResetErrorRecordsAttempt(t *testing.T) {
	f := &fakeSBProber{status: "not connected", resetErr: errors.New("appctl boom")}
	w := newTestWatchdog(f)
	base := time.Unix(1_000_000, 0)
	w.evaluate(context.Background(), base) // arm
	if !w.evaluate(context.Background(), base.Add(11*time.Second)) {
		t.Fatal("evaluate must report a reset attempt even when the reset errors")
	}
	if f.resets != 1 {
		t.Errorf("resets = %d, want 1 (attempt recorded despite the error)", f.resets)
	}
}

func withFastWatchdogInterval(t *testing.T) {
	t.Helper()
	prev := ovnWatchdogInterval
	ovnWatchdogInterval = time.Millisecond
	t.Cleanup(func() { ovnWatchdogInterval = prev })
}

// TestOVNWatchdog_LoopPollsAndStopsOnCancel drives runOVNControllerWatchdog (and the
// newOVNWatchdog constructor): it must poll on each tick and return promptly on
// context cancel.
func TestOVNWatchdog_LoopPollsAndStopsOnCancel(t *testing.T) {
	withFastWatchdogInterval(t)
	f := &fakeSBProber{status: "connected"}
	w := newOVNWatchdog(f)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		runOVNControllerWatchdog(ctx, w)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runOVNControllerWatchdog did not return on context cancel")
	}
	if f.calls == 0 {
		t.Error("watchdog loop never polled SBConnectionState")
	}
}
