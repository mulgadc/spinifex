package handlers_quota

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// staticResolver returns an InstanceTypeResolver serving one fixed answer, so a
// test drives EnforceRetype without a DescribeInstances round trip.
func staticResolver(instanceType string, ok bool, err error) InstanceTypeResolver {
	return func(string, string) (string, bool, error) {
		return instanceType, ok, err
	}
}

// TestEnforceRetype covers the check-before gate. The account counter already
// holds the instance's old vCPUs, so the gate charges only the grow delta:
// retype-up within headroom and at the limit pass and return the delta, one over
// rejects, retype-down and same-size return a non-positive delta left to
// reconcile, and an unknown type on either side or a missing instance is deferred
// to the daemon. Each case uses its own account so the embedded JetStream is
// started once for the whole table.
func TestEnforceRetype(t *testing.T) {
	s := newVCPUService(t, Limits{Enabled: true, VCPUs: 8}) // t3.micro=2, m5.xlarge=4

	tests := []struct {
		name      string
		account   string
		oldType   string
		newType   string
		resolveOK bool
		reserve   int // pre-charged vCPUs (includes the instance's old vCPUs)
		wantDelta int
		wantErr   string
	}{
		{"retype up within headroom", "100000000001", "t3.micro", "m5.xlarge", true, 2, 2, ""},
		{"retype up to limit", "100000000002", "t3.micro", "m5.xlarge", true, 6, 2, ""},
		{"retype up over limit", "100000000003", "t3.micro", "m5.xlarge", true, 7, 0, awserrors.ErrorResourceLimitExceeded},
		{"retype down left to reconcile", "100000000004", "m5.xlarge", "t3.micro", true, 4, -2, ""},
		{"same size no charge", "100000000005", "t3.micro", "c5.large", true, 2, 0, ""},
		{"unknown new type left to daemon", "100000000006", "t3.micro", "made.up", true, 2, 0, ""},
		{"unknown old type left to daemon", "100000000007", "made.up", "t3.micro", true, 2, 0, ""},
		{"instance not found", "100000000008", "t3.micro", "m5.xlarge", false, 2, 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.reserve > 0 {
				if err := s.AddVCPU(t.Context(), tt.account, tt.reserve); err != nil {
					t.Fatalf("AddVCPU(%d): %v", tt.reserve, err)
				}
			}
			delta, err := s.EnforceRetype(t.Context(), staticResolver(tt.oldType, tt.resolveOK, nil), tt.account, "i-abc", tt.newType)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("EnforceRetype = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || err.Error() != tt.wantErr):
				t.Fatalf("EnforceRetype err = %v, want %q", err, tt.wantErr)
			}
			if delta != tt.wantDelta {
				t.Fatalf("EnforceRetype delta = %d, want %d", delta, tt.wantDelta)
			}
		})
	}
}

// TestRetypeChargesDelta mirrors the handler's two-phase flow: the gate returns
// the grow delta, then AddVCPU charges it so the counter reflects the new type.
// A retype from t3.micro (2) to m5.xlarge (4) on a 2/8 account lands at 4/8.
func TestRetypeChargesDelta(t *testing.T) {
	s := newVCPUService(t, Limits{Enabled: true, VCPUs: 8})
	if err := s.AddVCPU(t.Context(), testAccount, 2); err != nil { // the t3.micro is already counted
		t.Fatalf("seed AddVCPU: %v", err)
	}
	delta, err := s.EnforceRetype(t.Context(), staticResolver("t3.micro", true, nil), testAccount, "i-a", "m5.xlarge")
	if err != nil {
		t.Fatalf("EnforceRetype: %v", err)
	}
	if err := s.AddVCPU(t.Context(), testAccount, delta); err != nil {
		t.Fatalf("AddVCPU(%d): %v", delta, err)
	}
	assertCounter(t, s, testAccount, 4)
}

// TestRetypeDownDoesNotCharge confirms a shrink leaves the counter untouched: the
// gate returns a negative delta and AddVCPU no-ops it, so reconcile remains the
// only path that lowers the counter.
func TestRetypeDownDoesNotCharge(t *testing.T) {
	s := newVCPUService(t, Limits{Enabled: true, VCPUs: 8})
	if err := s.AddVCPU(t.Context(), testAccount, 4); err != nil { // the m5.xlarge is already counted
		t.Fatalf("seed AddVCPU: %v", err)
	}
	delta, err := s.EnforceRetype(t.Context(), staticResolver("m5.xlarge", true, nil), testAccount, "i-a", "t3.micro")
	if err != nil {
		t.Fatalf("EnforceRetype: %v", err)
	}
	if delta > 0 {
		t.Fatalf("delta = %d, want <= 0 (shrink left to reconcile)", delta)
	}
	if err := s.AddVCPU(t.Context(), testAccount, delta); err != nil {
		t.Fatalf("AddVCPU(%d): %v", delta, err)
	}
	assertCounter(t, s, testAccount, 4) // unchanged: reconcile lowers, not the retype
}

// TestEnforceRetypeResolverError surfaces a describe failure to the caller so the
// modify is rejected rather than charged on a guessed type.
func TestEnforceRetypeResolverError(t *testing.T) {
	s := newVCPUService(t, Limits{Enabled: true, VCPUs: 8})
	sentinel := errors.New("describe boom")
	delta, err := s.EnforceRetype(t.Context(), staticResolver("t3.micro", false, sentinel), testAccount, "i-a", "m5.xlarge")
	if !errors.Is(err, sentinel) || delta != 0 {
		t.Fatalf("EnforceRetype = (%d, %v), want (0, %v)", delta, err, sentinel)
	}
}

// Exempt callers (disabled config, system account) short-circuit before the
// resolver is consulted, so the gate returns a zero delta and the describe round
// trip is skipped entirely.
func TestEnforceRetypeExemptShortCircuits(t *testing.T) {
	failResolver := func(string, string) (string, bool, error) {
		t.Fatal("resolver called for exempt account")
		return "", false, nil
	}
	disabled := New(Limits{Enabled: false}, nil)
	enabled := New(Limits{Enabled: true, VCPUs: 8}, nil)
	cases := []struct {
		name string
		fn   func() (int, error)
	}{
		{"disabled", func() (int, error) {
			return disabled.EnforceRetype(t.Context(), failResolver, testAccount, "i-a", "m5.xlarge")
		}},
		{"system account", func() (int, error) {
			return enabled.EnforceRetype(t.Context(), failResolver, utils.GlobalAccountID, "i-a", "m5.xlarge")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			delta, err := tc.fn()
			if err != nil || delta != 0 {
				t.Fatalf("exempt EnforceRetype = (%d, %v), want (0, nil)", delta, err)
			}
		})
	}
}

// TestInstanceTypeFromReservations covers the resolver's extraction helper: it
// returns a live instance's type by ID, skips terminal instances, and reports
// absent for an unknown ID.
func TestInstanceTypeFromReservations(t *testing.T) {
	withID := func(id, instanceType, state string) *ec2.Instance {
		inst := instance(instanceType, state)
		inst.InstanceId = aws.String(id)
		return inst
	}
	reservations := []*ec2.Reservation{
		reservation(
			withID("i-term", "m5.xlarge", ec2.InstanceStateNameTerminated),
			withID("i-stopped", "t3.micro", ec2.InstanceStateNameStopped),
		),
	}
	tests := []struct {
		name       string
		instanceID string
		wantType   string
		wantOK     bool
	}{
		{"found stopped", "i-stopped", "t3.micro", true},
		{"terminal skipped", "i-term", "", false},
		{"absent", "i-missing", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotType, gotOK := instanceTypeFromReservations(reservations, tt.instanceID)
			if gotType != tt.wantType || gotOK != tt.wantOK {
				t.Fatalf("instanceTypeFromReservations(%q) = (%q, %v), want (%q, %v)",
					tt.instanceID, gotType, gotOK, tt.wantType, tt.wantOK)
			}
		})
	}
}

// TestConfirmInstanceType locks in the partial-view fix the production resolver
// relies on. A found instance resolves regardless of sweep completeness. A
// genuine absence on a complete sweep returns ok false with no error, so the gate
// charges nothing and the daemon rejects the modify; but an absence on an
// incomplete sweep fails closed with ErrorServerInternal, so a node or bucket that
// missed the sweep cannot be read as "absent" and wave a retype past the cap.
func TestConfirmInstanceType(t *testing.T) {
	withID := func(id, instanceType, state string) *ec2.Instance {
		inst := instance(instanceType, state)
		inst.InstanceId = aws.String(id)
		return inst
	}
	reservations := []*ec2.Reservation{
		reservation(withID("i-stopped", "t3.micro", ec2.InstanceStateNameStopped)),
	}
	tests := []struct {
		name       string
		instanceID string
		complete   bool
		wantType   string
		wantOK     bool
		wantErr    string
	}{
		{"found on complete sweep", "i-stopped", true, "t3.micro", true, ""},
		{"found on incomplete sweep", "i-stopped", false, "t3.micro", true, ""},
		{"absent on complete sweep defers to daemon", "i-missing", true, "", false, ""},
		{"absent on incomplete sweep fails closed", "i-missing", false, "", false, awserrors.ErrorServerInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotType, gotOK, err := confirmInstanceType(reservations, tt.instanceID, tt.complete)
			if gotType != tt.wantType || gotOK != tt.wantOK {
				t.Fatalf("confirmInstanceType() = (%q, %v), want (%q, %v)", gotType, gotOK, tt.wantType, tt.wantOK)
			}
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("confirmInstanceType() err = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || err.Error() != tt.wantErr):
				t.Fatalf("confirmInstanceType() err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
