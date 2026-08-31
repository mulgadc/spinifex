//test:in-package — the backoff is unexported state (guestPortBackoffBase /
//guestPortBackoffMax and the per-port failure map), and the tests drive
//ensureGuestPortDatapath and recordPortFailure directly.

package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/network/policy"
)

// withFastGuestPortBackoff shrinks the post-failure backoff so a test can cross
// it without sleeping for a minute.
func withFastGuestPortBackoff(t *testing.T, base, ceiling time.Duration) {
	t.Helper()
	b, m := guestPortBackoffBase, guestPortBackoffMax
	guestPortBackoffBase, guestPortBackoffMax = base, ceiling
	t.Cleanup(func() { guestPortBackoffBase, guestPortBackoffMax = b, m })
}

// TestEnsureGuestPortDatapath_BacksOffAfterFailure covers the zombie port: one
// whose guest is gone can never bind, so the second pass must cost nothing
// rather than paying the whole nudge sequence again.
func TestEnsureGuestPortDatapath_BacksOffAfterFailure(t *testing.T) {
	withFastGuestPortBounds(t)
	withFastGuestPortBackoff(t, time.Minute, time.Hour)

	f := &fakeClaimVerifier{guestUpAfter: -1} // never up
	r := &reconciler{gwClaim: f, ovn: ovnWithGuestLSP(t, "port-eni-1")}
	spec := policy.EIPSpec{VPCID: "vpc-a", PortName: "port-eni-1"}

	r.ensureGuestPortDatapath(context.Background(), spec)
	afterFirst := f.nudges
	if afterFirst == 0 {
		t.Fatal("first pass nudged 0 times, want the full sequence")
	}

	r.ensureGuestPortDatapath(context.Background(), spec)
	if f.nudges != afterFirst {
		t.Errorf("nudges = %d after the second pass, want %d: a port inside its backoff must not be re-probed",
			f.nudges, afterFirst)
	}
}

// TestEnsureGuestPortDatapath_BackoffGrowsAndCaps checks the interval doubles
// per consecutive failure and then holds, so a permanently dead port settles at
// a fixed low rate instead of one report per reconcile cycle.
func TestEnsureGuestPortDatapath_BackoffGrowsAndCaps(t *testing.T) {
	r := &reconciler{}
	withFastGuestPortBackoff(t, time.Minute, 8*time.Minute)

	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 8 * time.Minute}
	for i, w := range want {
		failures, backoff := r.recordPortFailure("port-eni-1")
		if failures != i+1 {
			t.Errorf("failure %d: count = %d, want %d", i+1, failures, i+1)
		}
		if backoff != w {
			t.Errorf("failure %d: backoff = %s, want %s", i+1, backoff, w)
		}
	}
}

// TestEnsureGuestPortDatapath_BackoffClearsOnConverge covers the guest coming
// back: the port must be probed again immediately once it binds, and the held
// backoff must not survive to delay the next real failure.
func TestEnsureGuestPortDatapath_BackoffClearsOnConverge(t *testing.T) {
	withFastGuestPortBounds(t)
	withFastGuestPortBackoff(t, time.Millisecond, time.Millisecond)

	f := &fakeClaimVerifier{guestUpAfter: -1}
	r := &reconciler{gwClaim: f, ovn: ovnWithGuestLSP(t, "port-eni-1")}
	spec := policy.EIPSpec{VPCID: "vpc-a", PortName: "port-eni-1"}

	r.ensureGuestPortDatapath(context.Background(), spec)
	if _, held := r.portBackoffUntil(spec.PortName); held {
		time.Sleep(2 * time.Millisecond)
	}

	// The guest comes back.
	f.guestUpAfter = 0
	f.nudges = 0
	r.ensureGuestPortDatapath(context.Background(), spec)

	if _, held := r.portBackoffUntil(spec.PortName); held {
		t.Error("backoff still held after the port converged")
	}
	if _, ok := r.portBackoff[spec.PortName]; ok {
		t.Error("backoff entry not cleared after the port converged")
	}
}

// TestEniIDFromPort pins the attribution added to the failure log: the LSP name
// alone cannot be mapped back to a guest.
func TestEniIDFromPort(t *testing.T) {
	if got := eniIDFromPort("port-eni-6b9b6e142f20ed708"); got != "eni-6b9b6e142f20ed708" {
		t.Errorf("eniIDFromPort = %q, want %q", got, "eni-6b9b6e142f20ed708")
	}
}
