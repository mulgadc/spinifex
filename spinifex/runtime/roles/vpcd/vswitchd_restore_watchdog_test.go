package vpcd

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeFlowRestore scripts Held and counts releases.
type fakeFlowRestore struct {
	held     bool
	heldErr  error
	released int
}

func (f *fakeFlowRestore) Held(context.Context) (bool, error) { return f.held, f.heldErr }

func (f *fakeFlowRestore) Release(context.Context) error {
	f.released++
	f.held = false
	return nil
}

func newTestVswitchdWatchdog(f *fakeFlowRestore) *vswitchdWatchdog {
	return &vswitchdWatchdog{prober: f, heldAfter: 60 * time.Second}
}

// ovs-ctl's own restart holds the datapath for seconds while it restores
// flows; releasing it then would undo the restore the flag exists for.
func TestVswitchdWatchdog_ShortHoldIsLeftAlone(t *testing.T) {
	f := &fakeFlowRestore{held: true}
	w := newTestVswitchdWatchdog(f)
	base := time.Unix(1_000_000, 0)
	for _, after := range []time.Duration{0, 15 * time.Second, 45 * time.Second} {
		if w.evaluate(context.Background(), base.Add(after)) {
			t.Fatalf("released after %s, inside the hold window", after)
		}
	}
	if f.released != 0 {
		t.Fatalf("released %d times, want 0", f.released)
	}
}

func TestVswitchdWatchdog_ReleasesAHoldThatOutlastsTheWindow(t *testing.T) {
	f := &fakeFlowRestore{held: true}
	w := newTestVswitchdWatchdog(f)
	base := time.Unix(1_000_000, 0)
	w.evaluate(context.Background(), base)
	if !w.evaluate(context.Background(), base.Add(61*time.Second)) {
		t.Fatal("a datapath held past the window must be released")
	}
	if f.released != 1 {
		t.Fatalf("released %d times, want 1", f.released)
	}
}

// The window is a continuous hold. A datapath that forwarded in between was
// released by ovs-ctl or ovn-controller, so a new hold starts a new window.
func TestVswitchdWatchdog_ForwardingResetsTheWindow(t *testing.T) {
	f := &fakeFlowRestore{held: true}
	w := newTestVswitchdWatchdog(f)
	base := time.Unix(1_000_000, 0)
	w.evaluate(context.Background(), base)
	f.held = false
	w.evaluate(context.Background(), base.Add(30*time.Second))
	f.held = true
	if w.evaluate(context.Background(), base.Add(61*time.Second)) {
		t.Fatal("released on a hold that began 31s ago")
	}
}

func TestVswitchdWatchdog_ProbeErrorNeverReleases(t *testing.T) {
	f := &fakeFlowRestore{held: true, heldErr: errors.New("ovs-appctl failed")}
	w := newTestVswitchdWatchdog(f)
	base := time.Unix(1_000_000, 0)
	for i := range 5 {
		if w.evaluate(context.Background(), base.Add(time.Duration(i)*time.Minute)) {
			t.Fatalf("released on a failed probe (tick %d)", i)
		}
	}
}
