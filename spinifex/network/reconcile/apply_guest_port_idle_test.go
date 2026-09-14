package reconcile

//test:in-package — drives applyEIPs and reads the unexported idle and backoff
//state the skip is recorded in.

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/network/policy"
)

// captureReconcileLogs swaps the default logger for one writing to a buffer at
// Info, so a test can count what an operator would see in the journal.
func captureReconcileLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func idleEIPIntent(idle bool) IntentState {
	intent := IntentState{EIPs: map[string]policy.EIPSpec{
		"10.0.0.4": {VPCID: "vpc-a", ExternalIP: "192.0.2.46", LogicalIP: "10.0.0.4", PortName: "port-eni-1"},
	}}
	if idle {
		intent.IdlePorts = map[string]string{"port-eni-1": "i-stopped"}
	}
	return intent
}

// A customer-stopped instance keeps its ENI and EIP but has no tap, so its port
// can never bind. Passes must keep the DNAT row and never probe, nudge or file
// an ERROR, and say so once rather than every pass.
func TestApplyEIPs_StoppedInstancePortIsNotProbed(t *testing.T) {
	withFastGuestPortBounds(t)
	logs := captureReconcileLogs(t)

	f := &fakeClaimVerifier{guestUpAfter: -1}
	nat := &countingNAT{}
	r := &reconciler{gwClaim: f, nat: nat, ovn: ovnWithGuestLSP(t, "port-eni-1")}

	for range 3 {
		r.applyEIPs(context.Background(), idleEIPIntent(true), ActualState{}, &passResult{})
	}

	if got := nat.count(); got != 3 {
		t.Errorf("AddEIP called %d times, want 3: the DNAT row must still be asserted", got)
	}
	if f.guestChecks != 0 || f.nudges != 0 || f.sbChecks != 0 {
		t.Errorf("probes=%d nudges=%d sb_checks=%d, want all 0 for a stopped instance's port",
			f.guestChecks, f.nudges, f.sbChecks)
	}
	out := logs.String()
	if n := strings.Count(out, "guest port's instance is not running"); n != 1 {
		t.Errorf("idle notice logged %d times over 3 passes, want 1:\n%s", n, out)
	}
	for _, noisy := range []string{"level=WARN", "level=ERROR"} {
		if strings.Contains(out, noisy) {
			t.Errorf("stopped instance's port produced %s:\n%s", noisy, out)
		}
	}
}

// Starting the instance must converge on the first pass without a nudge, and
// the resume is logged once.
func TestApplyEIPs_StartedInstanceConvergesWithoutNudge(t *testing.T) {
	withFastGuestPortBounds(t)
	logs := captureReconcileLogs(t)

	f := &fakeClaimVerifier{guestUpAfter: 0}
	r := &reconciler{gwClaim: f, nat: &countingNAT{}, ovn: ovnWithGuestLSP(t, "port-eni-1")}

	r.applyEIPs(context.Background(), idleEIPIntent(true), ActualState{}, &passResult{})
	r.applyEIPs(context.Background(), idleEIPIntent(false), ActualState{}, &passResult{})
	r.applyEIPs(context.Background(), idleEIPIntent(false), ActualState{}, &passResult{})

	if f.guestChecks != 2 {
		t.Errorf("probes = %d, want 2: one per pass once the instance is running", f.guestChecks)
	}
	if f.nudges != 0 {
		t.Errorf("nudges = %d, want 0: a started instance's bound port needs no recompute", f.nudges)
	}
	out := logs.String()
	if n := strings.Count(out, "no longer stopped; probing its datapath again"); n != 1 {
		t.Errorf("resume notice logged %d times, want 1:\n%s", n, out)
	}
	if strings.Contains(out, "did not converge") {
		t.Errorf("started instance logged a convergence failure:\n%s", out)
	}
}

// Failures filed while the instance was stopping must not hold back the probe
// once it starts: backoff caps at 30 minutes, which would hide a real outage.
func TestApplyEIPs_IdleClearsHeldBackoff(t *testing.T) {
	withFastGuestPortBounds(t)
	withFastGuestPortBackoff(t, time.Hour, time.Hour)
	captureReconcileLogs(t)

	f := &fakeClaimVerifier{guestUpAfter: 0}
	r := &reconciler{gwClaim: f, nat: &countingNAT{}, ovn: ovnWithGuestLSP(t, "port-eni-1")}
	r.recordPortFailure("port-eni-1")

	r.applyEIPs(context.Background(), idleEIPIntent(true), ActualState{}, &passResult{})
	r.applyEIPs(context.Background(), idleEIPIntent(false), ActualState{}, &passResult{})

	if f.guestChecks != 1 {
		t.Errorf("probes = %d, want 1: the backoff from before the stop must not survive it", f.guestChecks)
	}
}

// An EIP that is released while its instance is stopped must not leave its idle
// mark behind to swallow a later notice for the same port.
func TestApplyEIPs_ForgetsIdleMarkWhenEIPGoes(t *testing.T) {
	withFastGuestPortBounds(t)
	captureReconcileLogs(t)

	r := &reconciler{gwClaim: &fakeClaimVerifier{guestUpAfter: 0}, nat: &countingNAT{}, ovn: ovnWithGuestLSP(t, "port-eni-1")}

	r.applyEIPs(context.Background(), idleEIPIntent(true), ActualState{}, &passResult{})
	r.applyEIPs(context.Background(), IntentState{}, ActualState{}, &passResult{})

	if len(r.idlePorts) != 0 {
		t.Errorf("idle marks = %v, want none once the port carries no public IP", r.idlePorts)
	}
}
