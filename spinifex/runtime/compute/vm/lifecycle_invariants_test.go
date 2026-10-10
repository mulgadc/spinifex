package vm

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRLC1_TerminateIdempotentOnAbsent enforces the Common Resource Lifecycle
// Contract rule #1 / ADR-0003 §2 (idempotent terminate): terminating an
// instance that is absent — or already terminated/shutting-down — is success,
// not a NotFound / invalid-transition error, so tofu destroy retries and the
// gateway's KV-health-gated idempotent path converge instead of looping.
func TestRLC1_TerminateIdempotentOnAbsent(t *testing.T) {
	cases := []struct {
		name  string
		setup func(m *Manager) string
	}{
		{"absent", func(*Manager) string { return "i-never-existed" }},
		{"terminated", func(m *Manager) string {
			v := &VM{ID: "i-already-terminated", Status: StateTerminated, Instance: &ec2.Instance{}}
			m.Insert(v)
			return v.ID
		}},
		{"shutting-down", func(m *Manager) string {
			v := &VM{ID: "i-already-shutting", Status: StateShuttingDown, Instance: &ec2.Instance{}}
			m.Insert(v)
			return v.ID
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, cleaner, rt, down, _ := terminateTestManager(t, newFakeStateStore())
			id := tc.setup(m)

			err := m.Terminate(id)

			require.NoErrorf(t, err, "Terminate of an %s instance must be idempotent success, not an error (RLC rule #1 / ADR-0003 §2): return nil so destroy retries converge", tc.name)
			assert.Emptyf(t, cleaner.deleteVolumes, "%s: idempotent terminate must run no cleanup", tc.name)
			assert.Zerof(t, down.Load(), "%s: idempotent terminate must not fire OnInstanceDown", tc.name)
			assert.Emptyf(t, rt.snapshot(), "%s: idempotent terminate must drive no state transition", tc.name)
		})
	}
}

// TestRLC4_TerminateStampsTeardownMap enforces ADR-0003 §1 (deterministic
// tracked teardown): a successful terminate must record a Teardown entry for
// every dependency it touches so the GC backstop (ADR-0003 §3) can later
// distinguish a fully-reaped instance from one with lingering async teardown.
// A missing mark is an untracked dependency — the reaper would purge the
// terminated record while an OVN port or NAT rule still dangles.
func TestRLC4_TerminateStampsTeardownMap(t *testing.T) {
	m, _, _, _, _ := terminateTestManager(t, newFakeStateStore())

	v := &VM{
		ID:                 "i-teardown-marks",
		Status:             StateRunning,
		InstanceType:       "t3.micro",
		ENIId:              "eni-abc",
		PublicIP:           "203.0.113.7",
		PlacementGroupName: "pg-1",
		Instance:           &ec2.Instance{},
	}
	m.Insert(v)

	require.NoError(t, m.Terminate(v.ID))

	// Sync steps that completed are Done; fire-and-forget async steps (OVN
	// LSP, NAT rule) are Pending until the reconciler/GC reaps them.
	want := map[string]TeardownState{
		TeardownQEMU:      TeardownDone,
		TeardownVolumes:   TeardownDone,
		TeardownTap:       TeardownDone,
		TeardownENI:       TeardownDone,
		TeardownOVN:       TeardownPending,
		TeardownNAT:       TeardownPending,
		TeardownPlacement: TeardownDone,
	}
	for dep, state := range want {
		assert.Equalf(t, string(state), v.Teardown[dep],
			"ADR-0003 §1: terminate must stamp Teardown[%q]=%s; an unstamped dependency is untracked and the GC reaper could purge the terminated record while it still dangles", dep, state)
	}
	// No GPU was attached, so the GPU dependency must not be stamped.
	_, gpuStamped := v.Teardown[TeardownGPU]
	assert.Falsef(t, gpuStamped, "ADR-0003 §1: dependency that does not apply (no GPU attached) must not be stamped")
}

// TestRLC4_TerminateMarksFailedDependency enforces ADR-0003 §1: a teardown
// step that errors is recorded as Failed, never silently dropped or marked
// Done. TeardownComplete must then report false so the GC backstop retains
// the record for a later sweep rather than declaring the instance fully reaped.
func TestRLC4_TerminateMarksFailedDependency(t *testing.T) {
	m, cleaner, _, _, _ := terminateTestManager(t, newFakeStateStore())
	cleaner.detachENIErr = errors.New("eni detach raced")

	v := &VM{
		ID:           "i-eni-fail",
		Status:       StateRunning,
		InstanceType: "t3.micro",
		ENIId:        "eni-fail",
		Instance:     &ec2.Instance{},
	}
	m.Insert(v)

	require.NoError(t, m.Terminate(v.ID))

	assert.Equalf(t, string(TeardownFailed), v.Teardown[TeardownENI],
		"ADR-0003 §1: a failed teardown step must be stamped Failed, not Done or dropped")
	assert.Falsef(t, v.TeardownComplete(),
		"ADR-0003 §1: TeardownComplete must be false while any dependency is Failed/Pending so the GC backstop retains the record for retry")
}
