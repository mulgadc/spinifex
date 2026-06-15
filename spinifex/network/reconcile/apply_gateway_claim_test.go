package reconcile

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeClaimVerifier scripts a sequence of GatewayPortClaimed results and counts
// NudgeRecompute calls. claimedAfter controls when the port flips to claimed:
// once nudgeCount reaches it (or immediately for 0), claimed reports true.
type fakeClaimVerifier struct {
	claimedAfter int // nudges required before the port reports claimed; <0 never
	checkErr     error
	nudgeErr     error
	checks       int
	nudges       int
	lastPort     string // most recent port name passed to GatewayPortClaimed
}

func (f *fakeClaimVerifier) GatewayPortClaimed(_ context.Context, port string) (bool, error) {
	f.checks++
	f.lastPort = port
	if f.checkErr != nil {
		return false, f.checkErr
	}
	if f.claimedAfter < 0 {
		return false, nil
	}
	return f.nudges >= f.claimedAfter, nil
}

func (f *fakeClaimVerifier) NudgeRecompute(_ context.Context) error {
	f.nudges++
	return f.nudgeErr
}

func withFastClaimBounds(t *testing.T) {
	t.Helper()
	to, iv := gatewayClaimTimeout, gatewayClaimInterval
	gatewayClaimTimeout = 200 * time.Millisecond
	gatewayClaimInterval = 1 * time.Millisecond
	t.Cleanup(func() { gatewayClaimTimeout, gatewayClaimInterval = to, iv })
}

func TestEnsureGatewayClaimed_NoVerifierIsNoop(t *testing.T) {
	r := &reconciler{} // gwClaim nil
	r.ensureGatewayClaimed(context.Background(), "gw-vpc-a")
	// Reaching here without panic is the assertion.
}

func TestEnsureGatewayClaimed_AlreadyClaimedNoNudge(t *testing.T) {
	withFastClaimBounds(t)
	f := &fakeClaimVerifier{claimedAfter: 0}
	r := &reconciler{gwClaim: f}

	r.ensureGatewayClaimed(context.Background(), "gw-vpc-a")

	if f.nudges != 0 {
		t.Errorf("claimed port nudged %d times, want 0", f.nudges)
	}
	if f.checks != 1 {
		t.Errorf("checks = %d, want 1 (single claimed read)", f.checks)
	}
}

func TestEnsureGatewayClaimed_NudgeThenConverge(t *testing.T) {
	withFastClaimBounds(t)
	// Unclaimed until one recompute nudge, then claimed.
	f := &fakeClaimVerifier{claimedAfter: 1}
	r := &reconciler{gwClaim: f}

	r.ensureGatewayClaimed(context.Background(), "gw-vpc-a")

	if f.nudges != 1 {
		t.Errorf("nudges = %d, want exactly 1 (nudge once, then converge)", f.nudges)
	}
}

func TestEnsureGatewayClaimed_NeverConvergesNudgesOnceThenGivesUp(t *testing.T) {
	withFastClaimBounds(t)
	f := &fakeClaimVerifier{claimedAfter: -1} // never claims
	r := &reconciler{gwClaim: f}

	done := make(chan struct{})
	go func() {
		r.ensureGatewayClaimed(context.Background(), "gw-vpc-a")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ensureGatewayClaimed did not return within deadline; blocking reconcile")
	}

	if f.nudges != 1 {
		t.Errorf("nudges = %d, want exactly 1 (nudge once, do not spam)", f.nudges)
	}
	if f.checks < 2 {
		t.Errorf("checks = %d, want >=2 (polled past the first nudge)", f.checks)
	}
}

func TestEnsureGatewayClaimed_CheckErrorBailsOut(t *testing.T) {
	withFastClaimBounds(t)
	f := &fakeClaimVerifier{checkErr: errors.New("ovn-sbctl down")}
	r := &reconciler{gwClaim: f}

	r.ensureGatewayClaimed(context.Background(), "gw-vpc-a")

	if f.nudges != 0 {
		t.Errorf("nudges = %d, want 0 (bail out on check error, do not nudge blindly)", f.nudges)
	}
}

func TestEnsureGatewayClaimed_ContextCancelStops(t *testing.T) {
	to, iv := gatewayClaimTimeout, gatewayClaimInterval
	gatewayClaimTimeout = 10 * time.Second
	gatewayClaimInterval = 50 * time.Millisecond
	t.Cleanup(func() { gatewayClaimTimeout, gatewayClaimInterval = to, iv })

	f := &fakeClaimVerifier{claimedAfter: -1}
	r := &reconciler{gwClaim: f}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		r.ensureGatewayClaimed(ctx, "gw-vpc-a")
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ensureGatewayClaimed ignored context cancellation")
	}
}
