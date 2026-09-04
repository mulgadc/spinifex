package reconcile

//test:in-package — drives applyEIPs and the unexported convergence bounds
//directly, neither of which is reachable from an external test package.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/network/policy"
)

// concurrentClaimVerifier reports livePort up and every other port permanently
// down, recording when the live port was first observed. Thread-safe because
// the ports are probed concurrently, which is the point of the test.
type concurrentClaimVerifier struct {
	livePort string

	mu       sync.Mutex
	nudges   int
	liveSeen time.Time
}

func (f *concurrentClaimVerifier) GuestPortUp(_ context.Context, lspName string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if lspName != f.livePort {
		return false, nil
	}
	if f.liveSeen.IsZero() {
		f.liveSeen = time.Now()
	}
	return true, nil
}

func (f *concurrentClaimVerifier) NudgeRecompute(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nudges++
	return nil
}

func (f *concurrentClaimVerifier) whenLiveSeen() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.liveSeen
}

func (f *concurrentClaimVerifier) GatewayPortClaimed(context.Context, string) (bool, error) {
	return true, nil
}
func (f *concurrentClaimVerifier) GatewayReachable(context.Context, string) (bool, error) {
	return true, nil
}
func (f *concurrentClaimVerifier) EIPReachable(context.Context, string) (bool, error) {
	return true, nil
}
func (f *concurrentClaimVerifier) RepairDatapath(context.Context) error { return nil }
func (f *concurrentClaimVerifier) SBConnectionState(context.Context) (string, error) {
	return "connected", nil
}
func (f *concurrentClaimVerifier) ResetSBClusterState(context.Context) error { return nil }

// countingNAT accepts every AddEIP. Only that method is exercised by applyEIPs,
// so the rest of NATManager is inherited and never called.
type countingNAT struct {
	policy.NATManager

	mu sync.Mutex
	n  int
}

func (c *countingNAT) AddEIP(context.Context, policy.EIPSpec) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return nil
}

func (c *countingNAT) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// ovnWithGuestLSPs returns a mock OVN client carrying every named port, which
// ensureGuestPortDatapath requires before it will probe.
func ovnWithGuestLSPs(t *testing.T, lspNames ...string) *mock.Client {
	t.Helper()
	m := mock.New()
	if err := m.Connect(context.Background()); err != nil {
		t.Fatalf("mock Connect: %v", err)
	}
	if err := m.CreateLogicalSwitch(context.Background(), &nbdb.LogicalSwitch{Name: "ls-test"}); err != nil {
		t.Fatalf("CreateLogicalSwitch: %v", err)
	}
	for _, name := range lspNames {
		if err := m.CreateLogicalSwitchPort(context.Background(), "ls-test", &nbdb.LogicalSwitchPort{Name: name}); err != nil {
			t.Fatalf("CreateLogicalSwitchPort %s: %v", name, err)
		}
	}
	return m
}

// TestApplyEIPs_HealthyPortDoesNotWaitForDeadOnes is the reproduction of the
// dev-prod symptom: three ports belonging to long-stopped instances each burned
// the full convergence deadline in turn, so a newly launched guest's EIP was
// dark for the sum of them before its own probe even started. The journal shows
// three consecutive 45s burns and the live guest's AddEIP landing only after
// the first one finished.
//
// The assertion is wall time, so it holds whatever order the intent map yields.
func TestApplyEIPs_HealthyPortDoesNotWaitForDeadOnes(t *testing.T) {
	withFastGuestPortBounds(t)

	const live = "port-eni-live"
	dead := []string{"port-eni-dead1", "port-eni-dead2", "port-eni-dead3"}

	f := &concurrentClaimVerifier{livePort: live}
	nat := &countingNAT{}
	r := &reconciler{
		gwClaim: f,
		nat:     nat,
		ovn:     ovnWithGuestLSPs(t, append([]string{live}, dead...)...),
	}

	intent := IntentState{EIPs: map[string]policy.EIPSpec{
		"10.0.0.1": {VPCID: "vpc-a", ExternalIP: "192.0.2.1", LogicalIP: "10.0.0.1", PortName: live},
	}}
	for i, name := range dead {
		ip := "10.0.0." + string(rune('2'+i))
		intent.EIPs[ip] = policy.EIPSpec{VPCID: "vpc-a", ExternalIP: "192.0.2.9", LogicalIP: ip, PortName: name}
	}

	res := &passResult{}
	start := time.Now()
	r.applyEIPs(context.Background(), intent, ActualState{}, res)
	elapsed := time.Since(start)

	if got := nat.count(); got != len(dead)+1 {
		t.Errorf("AddEIP called %d times, want %d: every spec must still get its DNAT row", got, len(dead)+1)
	}

	seen := f.whenLiveSeen()
	if seen.IsZero() {
		t.Fatal("the live port was never probed")
	}
	if waited := seen.Sub(start); waited >= guestPortDatapathTimeout {
		t.Errorf("the live port waited %s before its first probe, which is at least one full deadline (%s): "+
			"it is queued behind an unrelated port instead of converging on its own schedule",
			waited, guestPortDatapathTimeout)
	}

	// Serially this is one deadline per dead port; concurrently it is one
	// deadline for all of them. Two is comfortably between the two outcomes.
	if budget := 2 * guestPortDatapathTimeout; elapsed >= budget {
		t.Errorf("applyEIPs took %s for %d dead ports, want under %s: the convergence waits are still serialised",
			elapsed, len(dead), budget)
	}
}

// TestEscalateSBReset_NotRepeatedWhileConcurrent covers the hazard the
// concurrency introduces: sb-cluster-state-reset re-syncs every chassis, so
// simultaneous guest-port probes must not each fire one.
func TestEscalateSBReset_NotRepeatedWhileConcurrent(t *testing.T) {
	prev := sbResetMinInterval
	sbResetMinInterval = time.Hour
	t.Cleanup(func() { sbResetMinInterval = prev })

	f := &countingSBVerifier{}
	r := &reconciler{gwClaim: f}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { r.escalateSBReset(context.Background()) })
	}
	wg.Wait()

	if got := f.resets(); got != 1 {
		t.Errorf("ResetSBClusterState called %d times, want 1: a cluster-wide reset must not be fired once per port", got)
	}
}

// countingSBVerifier reports the SB wedge and counts resets.
type countingSBVerifier struct {
	concurrentClaimVerifier

	resetMu sync.Mutex
	n       int
}

func (c *countingSBVerifier) SBConnectionState(context.Context) (string, error) {
	return "not connected", nil
}

func (c *countingSBVerifier) ResetSBClusterState(context.Context) error {
	c.resetMu.Lock()
	defer c.resetMu.Unlock()
	c.n++
	return nil
}

func (c *countingSBVerifier) resets() int {
	c.resetMu.Lock()
	defer c.resetMu.Unlock()
	return c.n
}
