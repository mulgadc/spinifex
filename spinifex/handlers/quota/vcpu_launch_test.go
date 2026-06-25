package handlers_quota

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// reservationOf builds a reservation of n instances of one type, modelling what
// the daemon returns from a launch so ChargeLaunch sums the actual vCPUs.
func reservationOf(instanceType string, n int) *ec2.Reservation {
	res := &ec2.Reservation{}
	for range n {
		res.Instances = append(res.Instances, &ec2.Instance{InstanceType: aws.String(instanceType)})
	}
	return res
}

// TestEnforceLaunch covers the check-before gate: the charge tested is the
// worst case maxCount * the type's vCPUs, an at-limit launch passes, one over
// rejects, headroom is respected, and an unknown type is left for the daemon to
// reject (charged nothing here). Each case uses its own account so the embedded
// JetStream is started once for the whole table.
func TestEnforceLaunch(t *testing.T) {
	s := newVCPUService(t, Limits{Enabled: true, VCPUs: 8}) // t3.micro = 2 vCPU

	tests := []struct {
		name         string
		account      string
		instanceType string
		maxCount     int
		reserve      int // pre-charged vCPUs
		wantErr      string
	}{
		{"empty to limit", "100000000001", "t3.micro", 4, 0, ""},
		{"empty over limit", "100000000002", "t3.micro", 5, 0, awserrors.ErrorResourceLimitExceeded},
		{"headroom respected", "100000000003", "t3.micro", 1, 6, ""},
		{"headroom exceeded", "100000000004", "t3.micro", 2, 6, awserrors.ErrorResourceLimitExceeded},
		{"unknown type passes", "100000000005", "made.up", 1000, 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.reserve > 0 {
				if err := s.AddVCPU(tt.account, tt.reserve); err != nil {
					t.Fatalf("AddVCPU(%d): %v", tt.reserve, err)
				}
			}
			err := s.EnforceLaunch(tt.account, tt.instanceType, tt.maxCount)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("EnforceLaunch(%s, %d) = %v, want nil", tt.instanceType, tt.maxCount, err)
			case tt.wantErr != "" && (err == nil || err.Error() != tt.wantErr):
				t.Fatalf("EnforceLaunch(%s, %d) = %v, want %q", tt.instanceType, tt.maxCount, err, tt.wantErr)
			}
		})
	}
}

// TestChargeLaunchActual charges the vCPUs the daemon actually launched, not the
// checked worst case: an EnforceLaunch sized for maxCount=4 passes, but when the
// daemon returns only 2 instances the counter rises by their vCPUs alone.
func TestChargeLaunchActual(t *testing.T) {
	s := newVCPUService(t, Limits{Enabled: true, VCPUs: 8})

	if err := s.EnforceLaunch(testAccount, "t3.micro", 4); err != nil {
		t.Fatalf("EnforceLaunch = %v, want nil", err)
	}
	// Daemon launched only 2; charge the actual 4 vCPUs, not the checked 8.
	if err := s.ChargeLaunch(testAccount, reservationOf("t3.micro", 2)); err != nil {
		t.Fatalf("ChargeLaunch = %v", err)
	}
	got, _, err := s.readVCPU(testAccount)
	if err != nil {
		t.Fatalf("readVCPU: %v", err)
	}
	if got != 4 {
		t.Fatalf("counter = %d, want 4 (actual launched, not the checked worst case)", got)
	}
}

// TestLaunchSoftCapBounded documents the soft-cap window: two launches read the
// counter before either charges, so both pass the check and the counter then
// overshoots the cap — but only by the in-flight launches. CAS loses no update,
// so the final counter is exactly their sum, never unbounded.
func TestLaunchSoftCapBounded(t *testing.T) {
	s := newVCPUService(t, Limits{Enabled: true, VCPUs: 8})

	// Pre-charge to 6/8 so each 2-vCPU launch individually fits the headroom.
	if err := s.AddVCPU(testAccount, 6); err != nil {
		t.Fatalf("AddVCPU(6): %v", err)
	}

	// Two launches enter the gate before either charges: both read 6/8 and pass.
	if err := s.EnforceLaunch(testAccount, "t3.micro", 1); err != nil {
		t.Fatalf("first EnforceLaunch = %v, want nil in soft-cap window", err)
	}
	if err := s.EnforceLaunch(testAccount, "t3.micro", 1); err != nil {
		t.Fatalf("second EnforceLaunch = %v, want nil in soft-cap window", err)
	}

	// Both charges then land under CAS without losing an update.
	for range 2 {
		if err := s.ChargeLaunch(testAccount, reservationOf("t3.micro", 1)); err != nil {
			t.Fatalf("ChargeLaunch = %v", err)
		}
	}

	got, _, err := s.readVCPU(testAccount)
	if err != nil {
		t.Fatalf("readVCPU: %v", err)
	}
	// 6 + 2*2 = 10: the cap of 8 is overshot by exactly one launch and no more.
	if got != 10 {
		t.Fatalf("counter = %d, want 10 (soft-cap overshoot must stay bounded)", got)
	}
}

// Exempt callers must short-circuit before the counter is read or written, so
// both gates return nil even with a nil KV handle. Covers the disabled-config
// and system-account exemptions for the check and the charge.
func TestLaunchExemptShortCircuits(t *testing.T) {
	disabled := New(Limits{Enabled: false}, nil)
	enabled := New(Limits{Enabled: true, VCPUs: 8}, nil)
	cases := []struct {
		name string
		fn   func() error
	}{
		{"enforce disabled", func() error { return disabled.EnforceLaunch(testAccount, "t3.micro", 1000) }},
		{"charge disabled", func() error { return disabled.ChargeLaunch(testAccount, reservationOf("t3.micro", 1000)) }},
		{"enforce system account", func() error { return enabled.EnforceLaunch(utils.GlobalAccountID, "t3.micro", 1000) }},
		{"charge system account", func() error { return enabled.ChargeLaunch(utils.GlobalAccountID, reservationOf("t3.micro", 1000)) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.fn(); err != nil {
				t.Fatalf("exempt path returned %v, want nil", err)
			}
		})
	}
}
