package reconcile

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeClaimVerifier scripts GatewayPortClaim results and counts NudgeRecompute
// calls. claimedAfter controls when the port flips to claimed: once nudges
// reaches it (or immediately for 0), claimed reports true; <0 never claims.
type fakeClaimVerifier struct {
	claimedAfter int
	checkErr     error
	nudgeErr     error
	checks       int
	nudges       int
}

func (f *fakeClaimVerifier) GatewayPortClaim(_ context.Context, _ string) (bool, string, error) {
	f.checks++
	if f.checkErr != nil {
		return false, "", f.checkErr
	}
	if f.claimedAfter < 0 {
		return false, "chassis : []", nil
	}
	claimed := f.nudges >= f.claimedAfter
	return claimed, "chassis : []", nil
}

func (f *fakeClaimVerifier) NudgeRecompute(_ context.Context) error {
	f.nudges++
	return f.nudgeErr
}

func withFastClaimBounds(t *testing.T) {
	t.Helper()
	orig := gatewayClaimRecheck
	gatewayClaimRecheck = 1 * time.Millisecond
	t.Cleanup(func() { gatewayClaimRecheck = orig })
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
		t.Errorf("checks = %d, want 1 (single claimed read, no recheck)", f.checks)
	}
}

func TestEnsureGatewayClaimed_NudgeOnceThenConverge(t *testing.T) {
	withFastClaimBounds(t)
	// Unclaimed on first read, claimed after the single recompute nudge.
	f := &fakeClaimVerifier{claimedAfter: 1}
	r := &reconciler{gwClaim: f}

	r.ensureGatewayClaimed(context.Background(), "gw-vpc-a")

	if f.nudges != 1 {
		t.Errorf("nudges = %d, want exactly 1", f.nudges)
	}
	if f.checks != 2 {
		t.Errorf("checks = %d, want 2 (initial read + one recheck)", f.checks)
	}
}

func TestEnsureGatewayClaimed_NeverConvergesNudgesOnceNoSpin(t *testing.T) {
	withFastClaimBounds(t)
	f := &fakeClaimVerifier{claimedAfter: -1} // never claims

	done := make(chan struct{})
	go func() {
		r := &reconciler{gwClaim: f}
		r.ensureGatewayClaimed(context.Background(), "gw-vpc-a")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ensureGatewayClaimed spun; must nudge once + recheck once and return")
	}

	if f.nudges != 1 {
		t.Errorf("nudges = %d, want exactly 1 (no spin, no repeated nudges)", f.nudges)
	}
	if f.checks != 2 {
		t.Errorf("checks = %d, want exactly 2 (initial read + one recheck, no loop)", f.checks)
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

func TestEnsureGatewayClaimed_NudgeErrorReturnsNoRecheck(t *testing.T) {
	withFastClaimBounds(t)
	f := &fakeClaimVerifier{claimedAfter: -1, nudgeErr: errors.New("ovn-appctl down")}
	r := &reconciler{gwClaim: f}

	r.ensureGatewayClaimed(context.Background(), "gw-vpc-a")

	if f.nudges != 1 {
		t.Errorf("nudges = %d, want 1", f.nudges)
	}
	if f.checks != 1 {
		t.Errorf("checks = %d, want 1 (nudge failed → no recheck)", f.checks)
	}
}

func TestEnsureGatewayClaimed_ContextCancelStops(t *testing.T) {
	orig := gatewayClaimRecheck
	gatewayClaimRecheck = 10 * time.Second // long, so cancel wins the recheck wait
	t.Cleanup(func() { gatewayClaimRecheck = orig })

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
		t.Fatal("ensureGatewayClaimed ignored context cancellation during recheck wait")
	}
}
