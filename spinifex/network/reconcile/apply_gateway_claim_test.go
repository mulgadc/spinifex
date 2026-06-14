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
// reachableAfter mirrors claimedAfter for the datapath probe.
type fakeClaimVerifier struct {
	claimedAfter    int // nudges required before the port reports claimed; <0 never
	reachableAfter  int // nudges required before the datapath reports reachable; <0 never
	guestBoundAfter int // probe calls before the guest port reports bound; <0 never
	checkErr        error
	nudgeErr        error
	reachErr        error
	guestErr        error
	checks          int
	nudges          int
	repairs         int
	reachChecks     int
	guestChecks     int
	lastPort        string // most recent port name passed to GatewayPortClaimed
	lastGwIP        string // most recent IP passed to GatewayReachable
	lastEIP         string // most recent IP passed to EIPReachable
	lastLSP         string // most recent lsp passed to GuestPortBound
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

// RepairDatapath re-asserts the uplink then recomputes. Shares the nudge counter so
// the reachableAfter scripting (nudges-to-recover) covers both gates uniformly.
func (f *fakeClaimVerifier) RepairDatapath(_ context.Context) error {
	f.repairs++
	f.nudges++
	return f.nudgeErr
}

func (f *fakeClaimVerifier) GatewayReachable(_ context.Context, gwIP string) (bool, error) {
	f.reachChecks++
	f.lastGwIP = gwIP
	if f.reachErr != nil {
		return false, f.reachErr
	}
	if f.reachableAfter < 0 {
		return false, nil
	}
	return f.nudges >= f.reachableAfter, nil
}

// EIPReachable shares the reachableAfter/reachErr scripting with GatewayReachable
// (the recover/give-up/error paths are identical regardless of probe target);
// lastEIP records the target so tests can assert the EIP path was taken.
func (f *fakeClaimVerifier) EIPReachable(_ context.Context, eip string) (bool, error) {
	f.reachChecks++
	f.lastEIP = eip
	if f.reachErr != nil {
		return false, f.reachErr
	}
	if f.reachableAfter < 0 {
		return false, nil
	}
	return f.nudges >= f.reachableAfter, nil
}

// GuestPortBound reports bound once the probe has been called more than
// guestBoundAfter times (immediately for 0; never for <0). It keys off the probe
// count, not nudges: the post-reboot logic polls the bind, then recomputes once.
// lastLSP records the probed port.
func (f *fakeClaimVerifier) GuestPortBound(_ context.Context, lspName string) (bool, error) {
	f.guestChecks++
	f.lastLSP = lspName
	if f.guestErr != nil {
		return false, f.guestErr
	}
	if f.guestBoundAfter < 0 {
		return false, nil
	}
	return f.guestChecks > f.guestBoundAfter, nil
}

func withFastGuestPortBounds(t *testing.T) {
	t.Helper()
	to, iv := guestPortDatapathTimeout, guestPortDatapathInterval
	guestPortDatapathTimeout = 200 * time.Millisecond
	guestPortDatapathInterval = 1 * time.Millisecond
	t.Cleanup(func() { guestPortDatapathTimeout, guestPortDatapathInterval = to, iv })
}

func withFastDatapathBounds(t *testing.T) {
	t.Helper()
	to, iv := gatewayDatapathTimeout, gatewayDatapathInterval
	gatewayDatapathTimeout = 200 * time.Millisecond
	gatewayDatapathInterval = 1 * time.Millisecond
	t.Cleanup(func() { gatewayDatapathTimeout, gatewayDatapathInterval = to, iv })
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

func TestEnsureGatewayDatapath_NoVerifierIsNoop(t *testing.T) {
	r := &reconciler{} // gwClaim nil
	r.ensureGatewayDatapath(context.Background(), "vpc-a", "192.168.1.241", "")
	// Reaching here without panic is the assertion.
}

func TestEnsureGatewayDatapath_EmptyIPIsNoop(t *testing.T) {
	f := &fakeClaimVerifier{}
	r := &reconciler{gwClaim: f}

	r.ensureGatewayDatapath(context.Background(), "vpc-a", "", "")

	if f.reachChecks != 0 {
		t.Errorf("reachChecks = %d, want 0 (no probe target must skip the probe)", f.reachChecks)
	}
}

func TestEnsureGatewayDatapath_ReachableNoNudge(t *testing.T) {
	withFastDatapathBounds(t)
	f := &fakeClaimVerifier{reachableAfter: 0}
	r := &reconciler{gwClaim: f}

	r.ensureGatewayDatapath(context.Background(), "vpc-a", "192.168.1.241", "")

	if f.nudges != 0 {
		t.Errorf("reachable datapath nudged %d times, want 0", f.nudges)
	}
	if f.reachChecks != 1 {
		t.Errorf("reachChecks = %d, want 1 (single reachable probe)", f.reachChecks)
	}
	if f.lastGwIP != "192.168.1.241" {
		t.Errorf("lastGwIP = %q, want 192.168.1.241", f.lastGwIP)
	}
}

func TestEnsureGatewayDatapath_NudgeThenRecover(t *testing.T) {
	withFastDatapathBounds(t)
	// Unreachable until one recompute nudge, then reachable.
	f := &fakeClaimVerifier{reachableAfter: 1}
	r := &reconciler{gwClaim: f}

	r.ensureGatewayDatapath(context.Background(), "vpc-a", "192.168.1.241", "")

	if f.repairs != 1 {
		t.Errorf("repairs = %d, want exactly 1 (repair once, then recover)", f.repairs)
	}
	if f.nudges != 1 {
		t.Errorf("nudges = %d, want exactly 1 (repair includes a recompute)", f.nudges)
	}
}

func TestEnsureGatewayDatapath_NeverRecoversNudgesOnceThenGivesUp(t *testing.T) {
	withFastDatapathBounds(t)
	f := &fakeClaimVerifier{reachableAfter: -1} // never reachable
	r := &reconciler{gwClaim: f}

	done := make(chan struct{})
	go func() {
		r.ensureGatewayDatapath(context.Background(), "vpc-a", "192.168.1.241", "")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ensureGatewayDatapath did not return within deadline; blocking reconcile")
	}

	if f.repairs != 1 {
		t.Errorf("repairs = %d, want exactly 1 (repair once, do not spam)", f.repairs)
	}
	if f.reachChecks < 2 {
		t.Errorf("reachChecks = %d, want >=2 (polled past the first repair)", f.reachChecks)
	}
}

func TestEnsureGatewayDatapath_ProbeErrorBailsOut(t *testing.T) {
	withFastDatapathBounds(t)
	f := &fakeClaimVerifier{reachErr: errors.New("ping unavailable")}
	r := &reconciler{gwClaim: f}

	r.ensureGatewayDatapath(context.Background(), "vpc-a", "192.168.1.241", "")

	if f.nudges != 0 {
		t.Errorf("nudges = %d, want 0 (bail out on probe error, do not nudge blindly)", f.nudges)
	}
}

func TestEnsureGatewayDatapath_ContextCancelStops(t *testing.T) {
	to, iv := gatewayDatapathTimeout, gatewayDatapathInterval
	gatewayDatapathTimeout = 10 * time.Second
	gatewayDatapathInterval = 50 * time.Millisecond
	t.Cleanup(func() { gatewayDatapathTimeout, gatewayDatapathInterval = to, iv })

	f := &fakeClaimVerifier{reachableAfter: -1}
	r := &reconciler{gwClaim: f}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		r.ensureGatewayDatapath(ctx, "vpc-a", "192.168.1.241", "")
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ensureGatewayDatapath ignored context cancellation")
	}
}

func TestEnsureGatewayDatapath_PrefersEIPProbe(t *testing.T) {
	withFastDatapathBounds(t)
	f := &fakeClaimVerifier{reachableAfter: 0}
	r := &reconciler{gwClaim: f}

	r.ensureGatewayDatapath(context.Background(), "vpc-a", "192.168.1.241", "203.0.113.5")

	if f.lastEIP != "203.0.113.5" {
		t.Errorf("lastEIP = %q, want 203.0.113.5 (EIP must be the probe target when present)", f.lastEIP)
	}
	if f.lastGwIP != "" {
		t.Errorf("lastGwIP = %q, want empty (LRP probe must not run when an EIP is present)", f.lastGwIP)
	}
}

func TestEnsureGatewayDatapath_EIPUnreachableRepairs(t *testing.T) {
	withFastDatapathBounds(t)
	// EIP unreachable until one repair-recompute, then reachable.
	f := &fakeClaimVerifier{reachableAfter: 1}
	r := &reconciler{gwClaim: f}

	r.ensureGatewayDatapath(context.Background(), "vpc-a", "192.168.1.241", "203.0.113.5")

	if f.repairs != 1 {
		t.Errorf("repairs = %d, want exactly 1 (a stranded EIP datapath must trigger repair)", f.repairs)
	}
	if f.lastEIP != "203.0.113.5" {
		t.Errorf("lastEIP = %q, want 203.0.113.5", f.lastEIP)
	}
}

func TestEnsureGuestPortDatapath_NoVerifierIsNoop(t *testing.T) {
	r := &reconciler{} // gwClaim nil
	r.ensureGuestPortDatapath(context.Background(), "vpc-a", "port-eni-1")
	// Reaching here without panic is the assertion.
}

func TestEnsureGuestPortDatapath_EmptyLSPIsNoop(t *testing.T) {
	f := &fakeClaimVerifier{}
	r := &reconciler{gwClaim: f}

	r.ensureGuestPortDatapath(context.Background(), "vpc-a", "")

	if f.guestChecks != 0 {
		t.Errorf("guestChecks = %d, want 0 (empty lsp must skip the probe)", f.guestChecks)
	}
}

func TestEnsureGuestPortDatapath_BoundRecomputesOnce(t *testing.T) {
	withFastGuestPortBounds(t)
	f := &fakeClaimVerifier{guestBoundAfter: 0} // bound on first probe
	r := &reconciler{gwClaim: f}

	r.ensureGuestPortDatapath(context.Background(), "vpc-a", "port-eni-1")

	if f.nudges != 1 {
		t.Errorf("bound guest port nudged %d times, want exactly 1 (recompute once after bind)", f.nudges)
	}
	if f.guestChecks != 1 {
		t.Errorf("guestChecks = %d, want 1 (single bind probe)", f.guestChecks)
	}
	if f.lastLSP != "port-eni-1" {
		t.Errorf("lastLSP = %q, want port-eni-1", f.lastLSP)
	}
}

func TestEnsureGuestPortDatapath_PollsThenBoundRecomputesOnce(t *testing.T) {
	withFastGuestPortBounds(t)
	// Unbound for one probe, then bound. The recompute fires only after the bind.
	f := &fakeClaimVerifier{guestBoundAfter: 1}
	r := &reconciler{gwClaim: f}

	r.ensureGuestPortDatapath(context.Background(), "vpc-a", "port-eni-1")

	if f.nudges != 1 {
		t.Errorf("nudges = %d, want exactly 1 (recompute once, after the port binds)", f.nudges)
	}
	if f.guestChecks != 2 {
		t.Errorf("guestChecks = %d, want 2 (one miss, then bound)", f.guestChecks)
	}
}

func TestEnsureGuestPortDatapath_NeverBoundNoRecompute(t *testing.T) {
	withFastGuestPortBounds(t)
	f := &fakeClaimVerifier{guestBoundAfter: -1} // never binds
	r := &reconciler{gwClaim: f}

	done := make(chan struct{})
	go func() {
		r.ensureGuestPortDatapath(context.Background(), "vpc-a", "port-eni-1")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ensureGuestPortDatapath did not return within deadline; blocking reconcile")
	}

	// Recompute only after a real bind: a port that never binds cannot be realised,
	// so nudging would just churn flows (the #351 EIP-darkening regression).
	if f.nudges != 0 {
		t.Errorf("nudges = %d, want 0 (no recompute without a bind)", f.nudges)
	}
	if f.guestChecks < 2 {
		t.Errorf("guestChecks = %d, want >=2 (polled to the deadline)", f.guestChecks)
	}
}

func TestEnsureGuestPortDatapath_ProbeErrorBailsOut(t *testing.T) {
	withFastGuestPortBounds(t)
	f := &fakeClaimVerifier{guestErr: errors.New("ovn-sbctl down")}
	r := &reconciler{gwClaim: f}

	r.ensureGuestPortDatapath(context.Background(), "vpc-a", "port-eni-1")

	if f.nudges != 0 {
		t.Errorf("nudges = %d, want 0 (bail out on probe error, do not nudge blindly)", f.nudges)
	}
}

func TestEnsureGuestPortDatapath_ContextCancelStops(t *testing.T) {
	to, iv := guestPortDatapathTimeout, guestPortDatapathInterval
	guestPortDatapathTimeout = 10 * time.Second
	guestPortDatapathInterval = 50 * time.Millisecond
	t.Cleanup(func() { guestPortDatapathTimeout, guestPortDatapathInterval = to, iv })

	f := &fakeClaimVerifier{guestBoundAfter: -1}
	r := &reconciler{gwClaim: f}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		r.ensureGuestPortDatapath(ctx, "vpc-a", "port-eni-1")
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ensureGuestPortDatapath ignored context cancellation")
	}
}
