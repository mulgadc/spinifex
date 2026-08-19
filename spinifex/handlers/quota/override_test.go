package handlers_quota

import (
	"encoding/json"
	"testing"
)

func ptr(v int) *int { return &v }

// baseLimits is the sandbox baseline every account inherits when it has no
// override, mirroring the shipped [quota] block.
func baseLimits() Limits {
	return Limits{
		Enabled:       true,
		VCPUs:         16,
		VPCs:          4,
		Subnets:       16,
		EIPs:          4,
		Volumes:       16,
		VolumesGiB:    200,
		RDSInstances:  2,
		LoadBalancers: 2,
	}
}

// An override replaces only the dimensions it sets; every other dimension must
// come through from the configured baseline untouched.
func TestOverridesApplyIsSparse(t *testing.T) {
	got := Overrides{VCPUs: ptr(32), RDSInstances: ptr(8)}.apply(baseLimits())

	if got.VCPUs != 32 {
		t.Errorf("VCPUs = %d, want 32", got.VCPUs)
	}
	if got.RDSInstances != 8 {
		t.Errorf("RDSInstances = %d, want 8", got.RDSInstances)
	}
	want := baseLimits()
	if got.VPCs != want.VPCs || got.EIPs != want.EIPs || got.Volumes != want.Volumes ||
		got.VolumesGiB != want.VolumesGiB || got.Subnets != want.Subnets ||
		got.LoadBalancers != want.LoadBalancers {
		t.Errorf("unset dimensions changed: got %+v, want %+v", got, want)
	}
}

// An empty override must leave the baseline exactly as it is: this is the state
// of nearly every account and the reason no migration is needed.
func TestOverridesApplyEmptyInherits(t *testing.T) {
	if got := (Overrides{}).apply(baseLimits()); got != baseLimits() {
		t.Fatalf("empty override = %+v, want %+v", got, baseLimits())
	}
}

// Enablement is a cluster-wide switch, so no override may turn quotas off for
// one account.
func TestOverridesCannotDisableEnforcement(t *testing.T) {
	if got := (Overrides{VCPUs: ptr(0)}).apply(baseLimits()); !got.Enabled {
		t.Fatal("override cleared Enabled, want it preserved")
	}
}

// Zero is a real limit that denies every request, so it must survive a JSON
// round trip rather than marshalling away and silently meaning "inherit".
func TestOverridesZeroSurvivesRoundTrip(t *testing.T) {
	data, err := json.Marshal(Overrides{VCPUs: ptr(0)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var back Overrides
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.VCPUs == nil {
		t.Fatalf("explicit 0 was dropped by the round trip: %s", data)
	}
	if *back.VCPUs != 0 {
		t.Fatalf("VCPUs = %d, want 0", *back.VCPUs)
	}
	if got := back.apply(baseLimits()); got.VCPUs != 0 {
		t.Fatalf("applied VCPUs = %d, want 0", got.VCPUs)
	}
}

// An absent field must stay absent through a round trip, so inheritance is not
// silently converted into an explicit zero.
func TestOverridesAbsentStaysAbsent(t *testing.T) {
	data, err := json.Marshal(Overrides{VPCs: ptr(8)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var back Overrides
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.VCPUs != nil {
		t.Fatalf("unset VCPUs decoded as %d, want nil", *back.VCPUs)
	}
	if back.apply(baseLimits()).VCPUs != baseLimits().VCPUs {
		t.Fatal("unset VCPUs did not inherit the baseline")
	}
}

func TestOverridesEmpty(t *testing.T) {
	tests := []struct {
		name string
		over Overrides
		want bool
	}{
		{"zero value", Overrides{}, true},
		{"provenance only", Overrides{UpdatedBy: "operator"}, true},
		{"one dimension", Overrides{EIPs: ptr(1)}, false},
		{"explicit zero", Overrides{VCPUs: ptr(0)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.over.Empty(); got != tt.want {
				t.Fatalf("Empty() = %v, want %v", got, tt.want)
			}
		})
	}
}

// A nil override bucket is what a quota-disabled gateway holds; resolution must
// fall through to the configured limits rather than panicking.
func TestLimitsForWithoutBucket(t *testing.T) {
	svc := New(baseLimits(), nil)
	if got := svc.limitsFor(t.Context(), "123456789012"); got != baseLimits() {
		t.Fatalf("limitsFor = %+v, want the configured limits", got)
	}
}

// The Unlimited sentinel disables one dimension for one account, which zero
// cannot express because zero is a real limit that denies everything.
func TestExceedsUnlimited(t *testing.T) {
	if err := exceeds(9000, 9000, Unlimited); err != nil {
		t.Fatalf("exceeds with Unlimited = %v, want nil", err)
	}
	if err := exceeds(0, 1, 0); err == nil {
		t.Fatal("a zero cap allowed a request, want it denied")
	}
	if err := exceeds(0, 0, 0); err != nil {
		t.Fatalf("a zero cap denied a zero request: %v", err)
	}
}
